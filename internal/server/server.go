package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

type Options struct {
	Addr            string
	ReadTimeout     time.Duration
	RequestTimeout  time.Duration
	MaxBodyBytes    int64
	ShutdownTimeout time.Duration
	Redis           Pinger
	Postgres        Pinger
	Logger          *slog.Logger

	Router          *router.Router
	Providers       map[string]providers.Provider
	ProviderTimeout map[string]time.Duration
}

type Server struct {
	ln              net.Listener
	http            *http.Server
	shutdownTimeout time.Duration
	logger          *slog.Logger
}

// New binds the listener immediately, so a port-in-use error surfaces at
// construction time rather than only once Run is called.
func New(opts Options) (*Server, error) {
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen %s: %w", opts.Addr, err)
	}

	chat := chatDeps{
		router:          opts.Router,
		providers:       opts.Providers,
		providerTimeout: opts.ProviderTimeout,
	}
	mux := newMux(opts.Redis, opts.Postgres, chat)
	handler := withRequestID(withRequestTimeout(opts.RequestTimeout, withMaxBody(opts.MaxBodyBytes, mux)))

	return newServer(ln, handler, opts.ReadTimeout, opts.ShutdownTimeout, opts.Logger), nil
}

func newServer(ln net.Listener, handler http.Handler, readTimeout, shutdownTimeout time.Duration, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		ln: ln,
		http: &http.Server{
			Handler:     handler,
			ReadTimeout: readTimeout,
		},
		shutdownTimeout: shutdownTimeout,
		logger:          logger,
	}
}

// Addr returns the actual bound address, which matters when Options.Addr
// used port 0.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Run serves until ctx is cancelled, then drains in-flight requests up to
// shutdownTimeout before returning. The serve goroutine's own error (e.g. a
// listener failure unrelated to shutdown) takes priority if it arrives
// first; a shutdown-timeout error takes priority over a clean serve exit
// once shutdown has been requested.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		err := s.http.Serve(s.ln)
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.logger.Info("server: shutdown requested, draining in-flight requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()

		shutdownErr := s.http.Shutdown(shutdownCtx)
		serveErr := <-errCh // Serve() always returns once Shutdown closes the listener.
		if shutdownErr != nil {
			return fmt.Errorf("server: shutdown: %w", shutdownErr)
		}
		return serveErr
	}
}
