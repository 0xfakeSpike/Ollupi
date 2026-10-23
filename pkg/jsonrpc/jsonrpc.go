package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeServerError    = -32000
)

func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

type Handler interface {
	Call(method string, params json.RawMessage) (result any, after func(), err *Error)
	Notify(method string, params json.RawMessage)
}

type ResponseFunc func(result json.RawMessage, err *Error)

const writeTimeout = 10 * time.Second

type Conn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	nextID  int
	pending map[string]ResponseFunc
	outbox  [][]byte
	closed  bool
	ready   chan struct{}
}

func NewConn(ws *websocket.Conn) *Conn {
	return &Conn{ws: ws, pending: make(map[string]ResponseFunc), ready: make(chan struct{}, 1)}
}

func (c *Conn) Serve(ctx context.Context, h Handler) error {
	written := make(chan struct{})
	go func() {
		defer close(written)
		c.write()
	}()
	defer func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.wake()
		<-written
	}()
	for {
		kind, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		if kind != websocket.MessageText {
			c.reply(nil, nil, Errorf(CodeInvalidRequest, "messages must be text"))
			continue
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			c.reply(nil, nil, Errorf(CodeParseError, "%v", err))
			continue
		}
		switch {
		case m.JSONRPC != "2.0":
			c.reply(m.ID, nil, Errorf(CodeInvalidRequest, `jsonrpc must be "2.0"`))
		case m.Method != "" && m.ID != nil:
			result, after, rerr := h.Call(m.Method, m.Params)
			c.reply(m.ID, result, rerr)
			if rerr == nil && after != nil {
				after()
			}
		case m.Method != "":
			h.Notify(m.Method, m.Params)
		case m.ID != nil:
			c.response(m)
		default:
			c.reply(nil, nil, Errorf(CodeInvalidRequest, "not a request, notification, or response"))
		}
	}
}

func (c *Conn) Notify(method string, params any) {
	c.send(Message{JSONRPC: "2.0", Method: method, Params: encode(params)})
}

func (c *Conn) Request(method string, params any, onResponse ResponseFunc) {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprint(c.nextID)
	c.pending[id] = onResponse
	c.mu.Unlock()
	c.send(Message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: encode(params)})
}

func (c *Conn) response(m Message) {
	c.mu.Lock()
	onResponse, ok := c.pending[string(m.ID)]
	delete(c.pending, string(m.ID))
	c.mu.Unlock()
	if !ok {
		log.Printf("jsonrpc: response to unknown request %s", m.ID)
		return
	}
	onResponse(m.Result, m.Error)
}

func (c *Conn) reply(id json.RawMessage, result any, rerr *Error) {
	if id == nil {
		id = json.RawMessage("null")
	}
	m := Message{JSONRPC: "2.0", ID: id, Error: rerr}
	if rerr == nil {
		m.Result = encode(result)
	}
	c.send(m)
}

func (c *Conn) send(m Message) {
	data := encode(m)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.outbox = append(c.outbox, data)
	c.mu.Unlock()
	c.wake()
}

func (c *Conn) wake() {
	select {
	case c.ready <- struct{}{}:
	default:
	}
}

func (c *Conn) write() {
	for range c.ready {
		c.mu.Lock()
		batch, closed := c.outbox, c.closed
		c.outbox = nil
		c.mu.Unlock()
		for _, data := range batch {
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				log.Printf("jsonrpc: write: %v", err)
				c.ws.Close(websocket.StatusInternalError, "write failed")
				c.mu.Lock()
				c.closed, c.outbox = true, nil
				c.mu.Unlock()
				closed = true
				break
			}
		}
		if closed {
			return
		}
	}
}

func DecodeParams(params json.RawMessage, out any) *Error {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(params)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return Errorf(CodeInvalidParams, "%v", err)
	}
	return nil
}

func encode(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("jsonrpc: encode %T: %v", v, err))
	}
	return b
}
