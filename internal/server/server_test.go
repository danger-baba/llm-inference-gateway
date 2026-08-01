package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/config"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

type fakePinger struct {
	err error
}

func (f fakePinger) Ping(_ context.Context) error {
	return f.err
}

func TestHandleHealthz_AlwaysOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handleHealthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHandleReadyz(t *testing.T) {
	tests := []struct {
		name       string
		redis      Pinger
		postgres   Pinger
		wantStatus int
	}{
		{
			name:       "both healthy",
			redis:      fakePinger{},
			postgres:   fakePinger{},
			wantStatus: http.StatusOK,
		},
		{
			name:       "redis down",
			redis:      fakePinger{err: errors.New("dial tcp: connection refused")},
			postgres:   fakePinger{},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "postgres down",
			redis:      fakePinger{},
			postgres:   fakePinger{err: errors.New("pool exhausted")},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "both down",
			redis:      fakePinger{err: errors.New("redis down")},
			postgres:   fakePinger{err: errors.New("postgres down")},
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()

			handleReadyz(tt.redis, tt.postgres)(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func TestServer_GracefulShutdown_DrainsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	slowHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})

	ln := listenLocal(t)
	srv := newServer(ln, slowHandler, 5*time.Second, 2*time.Second, nil)
	addr := srv.Addr()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- srv.Run(ctx) }()

	reqDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			reqDone <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			reqDone <- fmt.Errorf("status = %d, want 200", resp.StatusCode)
			return
		}
		reqDone <- nil
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never reached the handler")
	}

	cancel() // request shutdown while the handler is still blocked
	time.Sleep(50 * time.Millisecond)
	close(release) // let the in-flight handler finish

	select {
	case err := <-reqDone:
		if err != nil {
			t.Fatalf("in-flight request did not complete cleanly: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never returned")
	}

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() never returned")
	}
}

func TestServer_GracefulShutdown_TimesOutOnStuckHandler(t *testing.T) {
	block := make(chan struct{}) // deliberately never closed
	stuckHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block
	})

	ln := listenLocal(t)
	srv := newServer(ln, stuckHandler, 5*time.Second, 100*time.Millisecond, nil)
	addr := srv.Addr()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- srv.Run(ctx) }()

	go func() {
		//nolint:bodyclose // the handler never responds; this connection is intentionally abandoned at test end
		_, _ = http.Get("http://" + addr + "/")
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-runErrCh:
		if err == nil {
			t.Fatal("Run() expected a shutdown-timeout error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() never returned")
	}
}

func TestNew_PortInUse_ReturnsError(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	addr := ln.Addr().String()

	_, err := New(Options{
		Addr:            addr,
		ReadTimeout:     time.Second,
		RequestTimeout:  time.Second,
		MaxBodyBytes:    1024,
		ShutdownTimeout: time.Second,
		Redis:           fakePinger{},
		Postgres:        fakePinger{},
	})
	if err == nil {
		t.Fatal("New() expected error for address already in use, got nil")
	}
}

func TestNew_MetricsEndpointServesRealCollectors(t *testing.T) {
	srv, err := New(Options{
		Addr:            "127.0.0.1:0",
		ReadTimeout:     time.Second,
		RequestTimeout:  time.Second,
		MaxBodyBytes:    1024,
		ShutdownTimeout: time.Second,
		Redis:           fakePinger{},
		Postgres:        fakePinger{},
		Router:          router.New(&config.Config{}),
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()

	resp, err := http.Get("http://" + srv.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// gateway_tokens_saved_total is a plain (non-Vec) counter, registered
	// eagerly with a zero value at package init -- unlike a CounterVec
	// (e.g. gateway_requests_total), which only appears in /metrics once
	// some other test happens to have called WithLabelValues on it, so
	// it's the one guaranteed to always be present regardless of test
	// execution order.
	if !bytes.Contains(body, []byte("gateway_tokens_saved_total")) {
		t.Errorf("/metrics body does not mention gateway_tokens_saved_total; got a %d-byte body", len(body))
	}
}
