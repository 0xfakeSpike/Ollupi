// Package jsonrpc is a JSON-RPC 2.0 connection over a WebSocket, usable in
// both directions: it dispatches the peer's requests and notifications to a
// Handler, and sends notifications and requests of its own, matching each
// response to the request it answers. It knows nothing about the methods.
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

// Message is one JSON-RPC 2.0 message: a request, notification, response,
// or error response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Error codes defined by JSON-RPC 2.0. CodeServerError starts the range
// reserved for implementation-defined server errors (-32000 to -32099).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeServerError    = -32000
)

// Errorf builds an error object.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Handler handles the peer's messages. Both methods run on the read loop, so
// they must not wait for the peer.
type Handler interface {
	// Call answers a request. after, when non-nil, runs once the response
	// has been sent.
	Call(method string, params json.RawMessage) (result any, after func(), err *Error)
	// Notify handles a notification.
	Notify(method string, params json.RawMessage)
}

// ResponseFunc receives the peer's response to a request: its result, or
// its error.
type ResponseFunc func(result json.RawMessage, err *Error)

const writeTimeout = 10 * time.Second

// Conn is one JSON-RPC connection.
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[string]ResponseFunc
}

// NewConn wraps an accepted WebSocket.
func NewConn(ws *websocket.Conn) *Conn {
	return &Conn{ws: ws, pending: make(map[string]ResponseFunc)}
}

// Serve reads messages until the connection fails or closes, and returns
// the read error.
func (c *Conn) Serve(ctx context.Context, h Handler) error {
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

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) {
	c.send(Message{JSONRPC: "2.0", Method: method, Params: encode(params)})
}

// Request sends a request; onResponse runs on the read loop when the
// response arrives. A request the peer never answers is never resolved.
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

// send writes one message. A connection that cannot be written is closed;
// its read loop then ends.
func (c *Conn) send(m Message) {
	data := encode(m)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		log.Printf("jsonrpc: write: %v", err)
		c.ws.Close(websocket.StatusInternalError, "write failed")
	}
}

// DecodeParams decodes request params strictly: unknown fields are an
// invalid-params error. Absent params decode as an empty object.
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
