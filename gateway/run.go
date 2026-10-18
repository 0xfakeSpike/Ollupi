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

	"github.com/0xfakeSpike/ollupi/memory"
)

// Options configures the gateway.
type Options struct {
	// Cron and RunCronJob are optional; without them the cron methods
	// answer that scheduled jobs are unavailable.
	Cron       Cron
	RunCronJob CronRunner
	Config     Config
	// Memory is optional, likewise.
	Memory memory.AdminStore
	// Token, when set, must be presented by every client as the "token"
	// query parameter or an "Authorization: Bearer" header.
	Token string
	// AllowedOrigins are the browser origins accepted besides localhost, as
	// host patterns (for example "wails.localhost").
	AllowedOrigins []string
}

// Serve serves the gateway on ln until ctx is cancelled, then closes every
// connection and returns once they have all ended.
func Serve(ctx context.Context, ln net.Listener, threads ThreadManager, opts Options) error {
	server := NewServer(threads, opts.Cron, opts.RunCronJob, opts.Config, opts.AllowedOrigins)
	server.SetMemoryStore(opts.Memory)
	mux := http.NewServeMux()
	mux.Handle("GET /", authorize(opts.Token, server))
	// Connection contexts derive from ctx, so shutdown closes every
	// WebSocket.
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
	server.Wait()
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
