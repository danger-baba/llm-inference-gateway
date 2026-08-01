package ledger

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testEntry() Entry {
	return Entry{
		RequestID: uuid.New(), OrgID: uuid.New(), TeamID: uuid.New(), VirtualKeyID: uuid.New(),
		Provider: "mock", Model: "mock-model-v1",
		PromptTokens: 10, CompletionTokens: 20, CacheTier: "none",
		Attempts: 1, StatusCode: 200, LatencyMS: 5,
	}
}

// fakeInserter records every InsertBatch call on calls (a copy of the
// batch, so later mutation of the caller's slice can't retroactively
// change what the test observes), signals entered on every call as soon
// as it starts (before any configured block), and optionally blocks on
// block until the test releases it -- letting tests deterministically
// synchronize with "the writer is now inside InsertBatch" instead of
// racing the background goroutine's scheduling.
type fakeInserter struct {
	entered chan struct{}
	block   chan struct{}
	calls   chan []Entry
}

func newFakeInserter() *fakeInserter {
	return &fakeInserter{entered: make(chan struct{}, 16), calls: make(chan []Entry, 16)}
}

func (f *fakeInserter) InsertBatch(_ context.Context, entries []Entry) error {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	if f.block != nil {
		<-f.block
	}
	cp := append([]Entry{}, entries...)
	f.calls <- cp
	return nil
}

func TestWriter_FlushesOnceBatchSizeReached(t *testing.T) {
	ins := newFakeInserter()
	w := NewWriter(ins, 10, 3, time.Hour, testLogger())
	defer func() { _ = w.Close(context.Background()) }()

	for i := 0; i < 3; i++ {
		w.Record(testEntry())
	}

	select {
	case batch := <-ins.calls:
		if len(batch) != 3 {
			t.Errorf("batch size = %d, want 3", len(batch))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("InsertBatch was never called after reaching batchSize")
	}
}

func TestWriter_FlushesPartialBatchOnInterval(t *testing.T) {
	ins := newFakeInserter()
	w := NewWriter(ins, 10, 100, 20*time.Millisecond, testLogger())
	defer func() { _ = w.Close(context.Background()) }()

	w.Record(testEntry())
	w.Record(testEntry())

	select {
	case batch := <-ins.calls:
		if len(batch) != 2 {
			t.Errorf("batch size = %d, want 2 (a partial batch flushed by the ticker)", len(batch))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ticker-triggered flush of a partial batch never happened")
	}
}

func TestWriter_NeverFlushesAnEmptyBatch(t *testing.T) {
	ins := newFakeInserter()
	w := NewWriter(ins, 10, 100, 20*time.Millisecond, testLogger())
	defer func() { _ = w.Close(context.Background()) }()

	// No Record calls at all; several ticks should pass with nothing to
	// send InsertBatch's way.
	select {
	case batch := <-ins.calls:
		t.Fatalf("InsertBatch called with %d entries, want no call at all when nothing was recorded", len(batch))
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWriter_DropsWhenBufferFull(t *testing.T) {
	ins := newFakeInserter()
	ins.block = make(chan struct{})
	w := NewWriter(ins, 2, 1, time.Hour, testLogger())
	defer func() { close(ins.block); _ = w.Close(context.Background()) }()

	droppedBefore := testutil.ToFloat64(metrics.LedgerDroppedTotal)

	w.Record(testEntry()) // dequeued almost immediately, then blocks inside InsertBatch
	<-ins.entered         // deterministic sync point: the channel is now empty again, 2 free slots

	w.Record(testEntry()) // fills slot 1
	w.Record(testEntry()) // fills slot 2 -- buffer now full
	w.Record(testEntry()) // must drop
	w.Record(testEntry()) // must drop

	droppedAfter := testutil.ToFloat64(metrics.LedgerDroppedTotal)
	if got := droppedAfter - droppedBefore; got != 2 {
		t.Errorf("gateway_ledger_dropped_total increased by %v, want 2", got)
	}
}

func TestWriter_RecordNeverBlocksEvenWhenFull(t *testing.T) {
	ins := newFakeInserter()
	ins.block = make(chan struct{}) // never released during this test
	w := NewWriter(ins, 1, 1, time.Hour, testLogger())
	defer close(ins.block)

	w.Record(testEntry())
	<-ins.entered

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			w.Record(testEntry()) // buffer capacity 1, already full after the first of these
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record() blocked instead of dropping once the buffer was full")
	}
}

func TestWriter_CloseDrainsBufferedEntries(t *testing.T) {
	ins := newFakeInserter()
	// batchSize and flushInterval both large enough that nothing flushes
	// on its own; only Close's drain should trigger the InsertBatch call.
	w := NewWriter(ins, 10, 100, time.Hour, testLogger())

	for i := 0; i < 3; i++ {
		w.Record(testEntry())
	}

	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	select {
	case batch := <-ins.calls:
		if len(batch) != 3 {
			t.Errorf("drained batch size = %d, want 3", len(batch))
		}
	default:
		t.Fatal("Close() returned without flushing the buffered entries")
	}
}

func TestWriter_CloseWithNothingBufferedDoesNotCallInsertBatch(t *testing.T) {
	ins := newFakeInserter()
	w := NewWriter(ins, 10, 100, time.Hour, testLogger())

	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case batch := <-ins.calls:
		t.Fatalf("InsertBatch called with %d entries on an empty Close, want no call", len(batch))
	default:
	}
}

type erroringInserter struct{ err error }

func (e erroringInserter) InsertBatch(context.Context, []Entry) error { return e.err }

func TestWriter_InsertBatchErrorDoesNotBlockShutdown(t *testing.T) {
	w := NewWriter(erroringInserter{err: errors.New("boom")}, 10, 1, time.Hour, testLogger())
	w.Record(testEntry())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close() error: %v, want nil -- an insert failure must not wedge shutdown", err)
	}
}
