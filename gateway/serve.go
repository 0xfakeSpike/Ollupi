package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
)

type Options struct {
	Methods        map[string]Method
	Token          string
	AllowedOrigins []string
}

func NewServer(a *agent.Agent, opts Options) (*Server, error) {
	s := &Server{
		agent:   a,
		token:   opts.Token,
		origins: append([]string{"localhost:*", "127.0.0.1:*", "[::1]:*"}, opts.AllowedOrigins...),
		conns:   make(map[*conn]struct{}),
		methods: make(map[string]Method),
	}
	for name, m := range opts.Methods {
		if err := s.Register(name, m); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.Handle("GET /", authorize(s.token, s))
	srv := &http.Server{Handler: mux, BaseContext: func(net.Listener) context.Context { return ctx }}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return fmt.Errorf("gateway: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("gateway shutdown: %v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Printf("gateway: %v", err)
	}
	s.served.Wait()
	return nil
}

func authorize(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("token")
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			got = bearer
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "invalid or missing gateway token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
