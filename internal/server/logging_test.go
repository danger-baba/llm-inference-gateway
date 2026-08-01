package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
)

func TestWithRequestLogging_EmitsRequestIDMethodPathStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := withRequestID(withRequestLogging(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logLine := buf.String()
	if logLine == "" {
		t.Fatal("no log line was written")
	}
	for _, want := range []string{`"status":418`, `"method":"GET"`, `"path":"/v1/models"`, `"request_id":"`} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("log line missing %q; got: %s", want, logLine)
		}
	}

	// The request ID in the log must be the same one returned to the
	// client, so a client-reported request_id can actually be found.
	wantID := rec.Header().Get("X-Request-Id")
	if wantID == "" || !bytes.Contains(buf.Bytes(), []byte(wantID)) {
		t.Errorf("log line does not contain the X-Request-Id header value %q; got: %s", wantID, logLine)
	}
}

func TestWithRequestLogging_StillFlushesThroughToStreamingHandler(t *testing.T) {
	// The statusCapturingWriter wrapper must not break http.Flusher --
	// if it did, every streaming response passed through logging would
	// silently degrade to "response writer cannot flush."
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	p := mock.New("mock-provider", 0, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)

	injectIdentity := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), identityCtxKey{}, testIdentity())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	handler := withRequestID(withRequestLogging(logger, injectIdentity(handleChatCompletions(deps))))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(
		`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q -- streaming broke when passed through logging middleware; body = %s", got, "text/event-stream", rec.Body.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"status":200`)) {
		t.Errorf("log line missing status 200; got: %s", buf.String())
	}
}

func TestMaybeLogPromptAndResponseBody_SilentByDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := mock.New("mock-provider", 0, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.logger = logger
	deps.logRequestBodies = false // the default

	const secret = "my extremely secret prompt content xyz123"
	body := `{"model":"fast","messages":[{"role":"user","content":"` + secret + `"}]}`
	doChatRequest(t, deps, body)

	if bytes.Contains(buf.Bytes(), []byte(secret)) {
		t.Fatalf("prompt text leaked into logs with the default config (log_request_bodies: false); log output: %s", buf.String())
	}
}

func TestMaybeLogPromptAndResponseBody_LogsWhenExplicitlyEnabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := mock.New("mock-provider", 0, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.logger = logger
	deps.logRequestBodies = true

	const secret = "my extremely secret prompt content xyz123"
	body := `{"model":"fast","messages":[{"role":"user","content":"` + secret + `"}]}`
	doChatRequest(t, deps, body)

	if !bytes.Contains(buf.Bytes(), []byte(secret)) {
		t.Errorf("prompt text did not appear in logs despite log_request_bodies: true; log output: %s", buf.String())
	}
}
