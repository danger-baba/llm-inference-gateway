package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/cache/exact"
	"github.com/danger-baba/llm-inference-gateway/internal/cache/semantic"
	"github.com/danger-baba/llm-inference-gateway/internal/embedding"
	"github.com/danger-baba/llm-inference-gateway/internal/ledger"
	"github.com/danger-baba/llm-inference-gateway/internal/retry"
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

	Router *router.Router
	Engine *retry.Engine

	AuthStore    *auth.Store
	AuthResolver *auth.CachingResolver

	TokenCounter             tokenCounter
	RateLimiter              rateLimiter
	DefaultTPM               int64
	EstimateCompletionTokens int64

	// ExactCache is nil when cache.exact.enabled is false.
	ExactCache              *exact.Store
	CacheNonzeroTemperature bool

	// Embedder and SemanticCache are nil when cache.semantic.enabled is
	// false, or when the embedding model failed to load — the README's
	// "disable this tier and continue with Tier-1" failure mode.
	Embedder      *embedding.Embedder
	SemanticCache *semantic.Store

	// Ledger and UsageAggregator are nil when Postgres isn't configured --
	// usage simply isn't recorded/queryable in that case, matching every
	// other Postgres-optional dependency in this struct.
	Ledger          *ledger.Writer
	UsageAggregator *ledger.PGAggregator
	// LogRequestBodies gates a separate, explicit debug-level log of
	// prompt/response content -- off by default, since prompts contain
	// user data (README, Observability).
	LogRequestBodies bool
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

	// Guard against the classic Go gotcha: assigning a nil *exact.Store
	// directly into an interface field would produce a non-nil interface
	// wrapping a nil pointer, so a later `!= nil` check in chat.go/admin.go
	// would wrongly conclude the cache is enabled.
	var cacheIface exactCache
	var purgerIface cachePurger
	if opts.ExactCache != nil {
		cacheIface = opts.ExactCache
		purgerIface = opts.ExactCache
	}
	var embedderIface embedder
	var semanticIface semanticCache
	if opts.Embedder != nil && opts.SemanticCache != nil {
		embedderIface = opts.Embedder
		semanticIface = opts.SemanticCache
	}
	var ledgerIface ledgerRecorder
	if opts.Ledger != nil {
		ledgerIface = opts.Ledger
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	chat := chatDeps{
		router:                   opts.Router,
		engine:                   opts.Engine,
		counter:                  opts.TokenCounter,
		limiter:                  opts.RateLimiter,
		defaultTPM:               opts.DefaultTPM,
		estimateCompletionTokens: opts.EstimateCompletionTokens,
		cache:                    cacheIface,
		cacheNonzeroTemperature:  opts.CacheNonzeroTemperature,
		embedder:                 embedderIface,
		semanticCache:            semanticIface,
		ledger:                   ledgerIface,
		logger:                   logger,
		logRequestBodies:         opts.LogRequestBodies,
	}
	var usageIface usageAggregator
	if opts.UsageAggregator != nil {
		usageIface = opts.UsageAggregator
	}
	admin := adminDeps{
		issuer:      opts.AuthStore,
		revoker:     opts.AuthStore,
		invalidator: opts.AuthResolver,
		purger:      purgerIface,
		usage:       usageIface,
	}
	mux := newMux(opts.Redis, opts.Postgres, chat, opts.AuthResolver, admin)
	handler := withRequestID(withRequestLogging(logger, withRequestTimeout(opts.RequestTimeout, withMaxBody(opts.MaxBodyBytes, mux))))

	return newServer(ln, handler, opts.ReadTimeout, opts.ShutdownTimeout, logger), nil
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
