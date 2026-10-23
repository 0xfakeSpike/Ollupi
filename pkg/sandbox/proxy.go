package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	proxyDialTimeout = 15 * time.Second
	maxBlockedHosts  = 20
)

type DomainRules struct {
	Allowed []string
	Denied  []string
}

type domainEntry struct {
	host string
	port int
}

func ParseDomains(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		e, err := parseDomainEntry(raw)
		if err != nil {
			return nil, err
		}
		s := e.host
		if e.port != 0 {
			s = net.JoinHostPort(e.host, strconv.Itoa(e.port))
		}
		out = append(out, s)
	}
	return out, nil
}

func parseDomainEntry(raw string) (domainEntry, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || strings.ContainsAny(s, "/@?# \t") || strings.Contains(s, "://") {
		return domainEntry{}, fmt.Errorf("sandbox: domain %q must be a host name, *.domain, *, or an IP address, with an optional :port", raw)
	}
	host, port := s, 0
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return domainEntry{}, fmt.Errorf("sandbox: domain %q has an invalid port", raw)
		}
		host, port = h, n
	}
	host = strings.Trim(host, "[]")
	if host != "*." {
		host = strings.TrimSuffix(host, ".")
	}
	switch {
	case host == "*":
	case strings.HasPrefix(host, "*."):
		if !validHostName(host[2:]) {
			return domainEntry{}, fmt.Errorf("sandbox: domain %q is not a valid wildcard", raw)
		}
	case net.ParseIP(host) != nil:
	case !validHostName(host) || strings.Contains(host, "*"):
		return domainEntry{}, fmt.Errorf("sandbox: domain %q is not a valid host name", raw)
	}
	return domainEntry{host: host, port: port}, nil
}

func validHostName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

type compiledRules struct {
	allow, deny []domainEntry
}

func compileRules(r DomainRules) (compiledRules, error) {
	var c compiledRules
	for _, list := range []struct {
		in  []string
		out *[]domainEntry
	}{{r.Allowed, &c.allow}, {r.Denied, &c.deny}} {
		for _, raw := range list.in {
			e, err := parseDomainEntry(raw)
			if err != nil {
				return compiledRules{}, err
			}
			*list.out = append(*list.out, e)
		}
	}
	return c, nil
}

func (e domainEntry) matches(host string, port int) bool {
	if e.port != 0 && e.port != port {
		return false
	}
	switch {
	case e.host == "*":
		return true
	case strings.HasPrefix(e.host, "*."):
		return strings.HasSuffix(host, e.host[1:])
	default:
		return host == e.host
	}
}

func (c compiledRules) decide(host string, port int) (allowed, literal bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	for _, e := range c.deny {
		if e.matches(host, port) {
			return false, false
		}
	}
	for _, e := range c.allow {
		if e.matches(host, port) {
			allowed = true
			if e.host == host {
				literal = true
			}
		}
	}
	return allowed, literal
}

func sensitiveAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast()
}

type proxy struct {
	ln      net.Listener
	rules   compiledRules
	ctx     context.Context
	cancel  context.CancelFunc
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	blocked []string
	seen    map[string]bool
	closed  bool
	wg      sync.WaitGroup
}

func startProxy(rules DomainRules) (*proxy, error) {
	compiled, err := compileRules(rules)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("sandbox: start network proxy: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &proxy{
		ln: ln, rules: compiled, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}, seen: map[string]bool{},
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

func (p *proxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *proxy) env() map[string]string {
	httpURL := "http://127.0.0.1:" + strconv.Itoa(p.port())
	socksURL := "socks5h://127.0.0.1:" + strconv.Itoa(p.port())
	return map[string]string{
		"HTTP_PROXY": httpURL, "HTTPS_PROXY": httpURL, "http_proxy": httpURL, "https_proxy": httpURL,
		"ALL_PROXY": socksURL, "all_proxy": socksURL,

		"NODE_USE_ENV_PROXY": "1",

		"GRPC_PROXY": httpURL, "grpc_proxy": httpURL,

		"JAVA_TOOL_OPTIONS": "-Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort=" + strconv.Itoa(p.port()) +
			" -Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=" + strconv.Itoa(p.port()) +

			" -Dhttp.nonProxyHosts= -Djava.net.preferIPv4Stack=true",

		"NO_PROXY": "", "no_proxy": "",
	}
}

func (p *proxy) close() {
	p.cancel()
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.ln.Close()
	p.wg.Wait()
}

func (p *proxy) blockedHosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.blocked...)
}

func (p *proxy) block(target, why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := "network: blocked " + target + " (" + why + ")"
	if p.seen[line] || len(p.blocked) >= maxBlockedHosts {
		return
	}
	p.seen[line] = true
	p.blocked = append(p.blocked, line)
}

func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

func (p *proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(c)
			br := bufio.NewReader(c)
			first, err := br.Peek(1)
			if err != nil {
				return
			}
			if first[0] == 0x05 {
				p.serveSOCKS(c, br)
				return
			}
			p.serveHTTP(c, br)
		}()
	}
}

func (p *proxy) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	allowed, literal := p.rules.decide(host, port)
	if !allowed {
		p.block(target, "not in allowed_domains")
		return nil, errBlocked
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addrs = []netip.Addr{a}
	} else {
		ctx, cancel := context.WithTimeout(ctx, proxyDialTimeout)
		defer cancel()
		if addrs, err = p.resolve(ctx, host); err != nil {
			return nil, err
		}
	}
	var lastErr error = fmt.Errorf("no address for %s", host)
	for _, a := range addrs {
		if sensitiveAddr(a) && !literal {
			p.block(target, "resolves to a local or link-local address; allow it by name or address to reach it")
			lastErr = errBlocked
			continue
		}
		d := net.Dialer{Timeout: proxyDialTimeout}
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

var errBlocked = errors.New("blocked by the sandbox's network allowlist")

func splitTarget(hostport, defaultPort string) (string, int, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, defaultPort
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 || host == "" {
		return "", 0, fmt.Errorf("invalid target %q", hostport)
	}
	return host, n, nil
}

func (p *proxy) serveHTTP(c net.Conn, br *bufio.Reader) {
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Method == http.MethodConnect {
			p.serveConnect(c, br, req)
			return
		}
		if !p.forward(c, req) {
			return
		}
	}
}

func (p *proxy) refuseHTTP(c net.Conn, err error) {
	status, reason := http.StatusBadGateway, err.Error()
	if errors.Is(err, errBlocked) {
		status = http.StatusForbidden
	}
	body := "sandbox: " + reason + "\n"
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, http.StatusText(status), len(body), body)
}

func (p *proxy) serveConnect(c net.Conn, br *bufio.Reader, req *http.Request) {
	host, port, err := splitTarget(req.Host, "443")
	if err != nil {
		p.refuseHTTP(c, err)
		return
	}
	up, err := p.dial(p.ctx, host, port)
	if err != nil {
		p.refuseHTTP(c, err)
		return
	}
	if !p.track(up) {
		return
	}
	defer p.untrack(up)
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	if n := br.Buffered(); n > 0 {
		buf, _ := br.Peek(n)
		if _, err := up.Write(buf); err != nil {
			return
		}
	}
	tunnel(c, up)
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *proxy) forward(c net.Conn, req *http.Request) bool {
	if req.URL == nil || req.URL.Scheme != "http" || req.URL.Host == "" {
		p.refuseHTTP(c, fmt.Errorf("a proxy request needs an absolute http:// URL; use CONNECT for https"))
		return false
	}
	host, port, err := splitTarget(req.URL.Host, "80")
	if err != nil {
		p.refuseHTTP(c, err)
		return false
	}
	up, err := p.dial(p.ctx, host, port)
	if err != nil {
		p.refuseHTTP(c, err)
		return false
	}
	if !p.track(up) {
		return false
	}
	defer p.untrack(up)
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.RequestURI = ""
	req.Close = true
	if err := req.Write(up); err != nil {
		p.refuseHTTP(c, err)
		return false
	}
	resp, err := http.ReadResponse(bufio.NewReader(up), req)
	if err != nil {
		p.refuseHTTP(c, err)
		return false
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		if h != "Transfer-Encoding" {
			resp.Header.Del(h)
		}
	}
	resp.Close = req.Close
	keep := !resp.Close
	if err := resp.Write(c); err != nil {
		return false
	}
	return keep
}

func (p *proxy) serveSOCKS(c net.Conn, br *bufio.Reader) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if !strings.ContainsRune(string(methods), 0x00) {
		c.Write([]byte{0x05, 0xff})
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	reply := func(code byte) { c.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) }
	if req[0] != 0x05 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = netip.AddrFrom4([4]byte(b)).String()
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = netip.AddrFrom16([16]byte(b)).String()
	case 0x03:
		n, err := br.ReadByte()
		if err != nil {
			return
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	default:
		reply(0x08)
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return
	}
	if req[1] != 0x01 {
		reply(0x07)
		return
	}
	port := int(binary.BigEndian.Uint16(pb))
	up, err := p.dial(p.ctx, host, port)
	if err != nil {
		if errors.Is(err, errBlocked) {
			reply(0x02)
		} else {
			reply(0x05)
		}
		return
	}
	if !p.track(up) {
		return
	}
	defer p.untrack(up)
	reply(0x00)
	if n := br.Buffered(); n > 0 {
		buf, _ := br.Peek(n)
		if _, err := up.Write(buf); err != nil {
			return
		}
	}
	tunnel(c, up)
}

func tunnel(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
