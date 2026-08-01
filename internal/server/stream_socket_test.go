package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
)

// TestHandleChatCompletions_Streaming_RealSocketDeliversIncrementally is
// the automated version of the README's own gate: "a streaming curl
// shows incremental output, not one buffered blob." httptest.Recorder
// (used by every other streaming test in this package) can't actually
// prove that, since it's an in-memory buffer where Flush is a no-op --
// this test instead runs the real handler behind a real net/http server
// on a real TCP socket, paces the fake provider's deltas over real wall-
// clock time, and asserts the client actually receives them spread out,
// not all at once after the fact.
func TestHandleChatCompletions_Streaming_RealSocketDeliversIncrementally(t *testing.T) {
	p := mock.New("mock-provider", 0, 0, 0)
	p.StreamDeltaLatency(25 * time.Millisecond)
	deps := testDepsWithProvider(t, "mock-provider", p)

	injectIdentity := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), identityCtxKey{}, testIdentity())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	srv := httptest.NewServer(injectIdentity(withRequestID(handleChatCompletions(deps))))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(
		`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q", got, "text/event-stream")
	}

	var arrivals []time.Time
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			arrivals = append(arrivals, time.Now())
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read response body: %v", err)
	}

	// "mock response to: hi" is 4 words, so 4 content lines + 1 [DONE].
	if len(arrivals) < 4 {
		t.Fatalf("got %d data lines over the wire, want at least 4", len(arrivals))
	}

	// The real proof: if the gateway buffered the whole response and
	// wrote it in one shot at the end, every line would arrive back to
	// back with ~0 gap between them. Because the fake provider paces
	// each delta 25ms apart, a genuinely unbuffered relay must show most
	// of that gap survive all the way to the client.
	first, last := arrivals[0], arrivals[len(arrivals)-1]
	gap := last.Sub(first)
	wantMinGap := 40 * time.Millisecond // well under 3*25ms, generous for scheduling jitter
	if gap < wantMinGap {
		t.Errorf("gap between first and last SSE line = %v, want >= %v -- looks buffered, not streamed", gap, wantMinGap)
	}
}
