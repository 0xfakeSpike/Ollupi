package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDomainRules(t *testing.T) {
	c, err := compileRules(DomainRules{
		Allowed: []string{"github.com", "*.npmjs.org", "api.example.com:8443", "localhost:3000"},
		Denied:  []string{"evil.npmjs.org"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host    string
		port    int
		allowed bool
		literal bool
	}{
		{"github.com", 443, true, true},
		{"GitHub.com.", 22, true, true},
		{"api.github.com", 443, false, false},
		{"registry.npmjs.org", 443, true, false},
		{"npmjs.org", 443, false, false},
		{"evil.npmjs.org", 443, false, false},
		{"api.example.com", 8443, true, true},
		{"api.example.com", 443, false, false},
		{"localhost", 3000, true, true},
		{"localhost", 22, false, false},
		{"example.com", 443, false, false},
	} {
		allowed, literal := c.decide(tc.host, tc.port)
		if allowed != tc.allowed || literal != tc.literal {
			t.Errorf("%s:%d = %v,%v want %v,%v", tc.host, tc.port, allowed, literal, tc.allowed, tc.literal)
		}
	}
	for _, bad := range []string{"", "https://github.com", "github.com/x", "a*b.com", "*.", "github.com:0", "user@host"} {
		if _, err := ParseDomains([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got, err := ParseDomains([]string{" GitHub.com ", "*", "[::1]:8080"}); err != nil || strings.Join(got, ",") != "github.com,*,[::1]:8080" {
		t.Errorf("got %v, %v", got, err)
	}
}

// echoServer answers each connection with what it was sent, prefixed.
func echoServer(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "echo:%s", line)
			}()
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// testProxy resolves every name to 127.0.0.1, standing in for the internet.
func testProxy(t *testing.T, rules DomainRules) *proxy {
	t.Helper()
	p, err := startProxy(rules)
	if err != nil {
		t.Fatal(err)
	}
	p.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	t.Cleanup(p.close)
	return p
}

func connectThrough(t *testing.T, p *proxy, target string) (net.Conn, string) {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p.port()))
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		c.Close()
		return nil, fmt.Sprintf("%d %s", resp.StatusCode, body)
	}
	return &bufConn{c, br}, "200"
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// A name that resolves to loopback or link-local (cloud metadata) is not
// reachable through a wildcard, only through an entry naming it.
func TestProxyConnectHonorsTheRules(t *testing.T) {
	_, port := echoServer(t)
	p := testProxy(t, DomainRules{Allowed: []string{"*.example.com", "allowed.test", "127.0.0.1:" + strconv.Itoa(port)}})

	c, status := connectThrough(t, p, "allowed.test:"+strconv.Itoa(port))
	if c == nil {
		t.Fatalf("allowed host refused: %s", status)
	}
	fmt.Fprint(c, "hi\n")
	if got, _ := bufio.NewReader(c).ReadString('\n'); got != "echo:hi\n" {
		t.Fatalf("tunnel carried %q", got)
	}
	c.Close()

	if c, status := connectThrough(t, p, "blocked.test:443"); c != nil || !strings.HasPrefix(status, "403") {
		t.Fatalf("blocked host: %s", status)
	}
	if c, status := connectThrough(t, p, "rebind.example.com:"+strconv.Itoa(port)); c != nil || !strings.HasPrefix(status, "403") {
		t.Fatalf("a wildcard reached loopback: %s", status)
	}
	if c, _ := connectThrough(t, p, "127.0.0.1:"+strconv.Itoa(port)); c == nil {
		t.Fatal("a literal loopback entry was refused")
	} else {
		c.Close()
	}
	blocked := strings.Join(p.blockedHosts(), "\n")
	if !strings.Contains(blocked, "blocked.test:443 (not in allowed_domains)") || !strings.Contains(blocked, "rebind.example.com") {
		t.Fatalf("blocked hosts not recorded: %s", blocked)
	}
}

func TestProxyForwardsPlainHTTP(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s proxy-auth=%q", r.URL.Path, r.Header.Get("Proxy-Authorization"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	p := testProxy(t, DomainRules{Allowed: []string{"localhost"}})
	proxyURL, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(p.port()))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	req, _ := http.NewRequest("GET", "http://localhost:"+strconv.Itoa(port)+"/hello", nil)
	req.Header.Set("Proxy-Authorization", "Basic leak")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `path=/hello proxy-auth=""` {
		t.Fatalf("got %q", body)
	}
	resp, err = client.Get("http://other.test/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked host got %d", resp.StatusCode)
	}
}

func TestProxySpeaksSOCKS5(t *testing.T) {
	_, port := echoServer(t)
	p := testProxy(t, DomainRules{Allowed: []string{"allowed.test"}})
	socks := func(host string) (net.Conn, byte) {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p.port()))
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte{5, 1, 0})
		hello := make([]byte, 2)
		if _, err := io.ReadFull(c, hello); err != nil || hello[1] != 0 {
			t.Fatalf("greeting: %v %v", hello, err)
		}
		req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
		req = binary.BigEndian.AppendUint16(req, uint16(port))
		c.Write(req)
		reply := make([]byte, 10)
		if _, err := io.ReadFull(c, reply); err != nil {
			t.Fatal(err)
		}
		return c, reply[1]
	}
	c, code := socks("allowed.test")
	if code != 0 {
		t.Fatalf("allowed host got reply %d", code)
	}
	fmt.Fprint(c, "s5\n")
	if got, _ := bufio.NewReader(c).ReadString('\n'); got != "echo:s5\n" {
		t.Fatalf("tunnel carried %q", got)
	}
	c.Close()
	if c, code := socks("blocked.test"); code != 2 {
		t.Fatalf("blocked host got reply %d", code)
	} else {
		c.Close()
	}
}

// Closing the proxy ends its tunnels, so nothing outlives the command.
func TestProxyCloseEndsTunnels(t *testing.T) {
	_, port := echoServer(t)
	p, err := startProxy(DomainRules{Allowed: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	c, status := connectThrough(t, p, "127.0.0.1:"+strconv.Itoa(port))
	if c == nil {
		t.Fatal(status)
	}
	done := make(chan struct{})
	go func() { p.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close hung on an open tunnel")
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("tunnel still open")
	}
}
