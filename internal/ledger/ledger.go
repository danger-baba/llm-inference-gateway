// Package ledger buffers per-request usage rows and flushes them to
// PostgreSQL in batches, so the request path never waits on a write to
// usage_ledger. Losing a billing row under sustained overload is an
// accepted trade-off (README, Cost accounting ledger / Failure modes):
// Record never blocks and drops silently (save for a counter) if the
// buffer is already full.
package ledger

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
)

// Entry is one row of usage_ledger. CacheTier is "none", "exact", or
// "semantic"; a cache hit is recorded with CostUSD 0 and TokensSaved set
// to what the call would have cost, which is what makes the cache's
// value measurable rather than assumed.
type Entry struct {
	RequestID        uuid.UUID
	OrgID            uuid.UUID
	TeamID           uuid.UUID
	VirtualKeyID     uuid.UUID
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TokensSaved      int
	CostUSD          float64
	CacheTier        string
	Attempts         int
	StatusCode       int
	LatencyMS        int
}

// Inserter is the narrow interface Writer needs; *PGInserter satisfies
// it against a real Postgres pool, and tests satisfy it with a fake so
// the buffering/batching/dropping logic can be verified without a live
// database.
type Inserter interface {
	InsertBatch(ctx context.Context, entries []Entry) error
}

// Writer owns the buffered channel and the background batch loop. The
// zero value is not usable; construct with NewWriter.
type Writer struct {
	entries       chan Entry
	ins           Inserter
	batchSize     int
	flushInterval time.Duration
	logger        *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

// NewWriter starts the background flush loop immediately. bufferSize
// bounds how many entries can queue before Record starts dropping;
// batchSize bounds how many rows go into one InsertBatch call;
// flushInterval bounds how long a partial batch can wait before it's
// sent anyway, so low-traffic periods still get written promptly rather
// than waiting for the buffer to fill.
func NewWriter(ins Inserter, bufferSize, batchSize int, flushInterval time.Duration, logger *slog.Logger) *Writer {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Writer{
		entries:       make(chan Entry, bufferSize),
		ins:           ins,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		logger:        logger,
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	go w.run(ctx)
	return w
}

// Record enqueues e for eventual batch insertion. Never blocks: a full
// buffer drops e and increments gateway_ledger_dropped_total rather than
// letting a slow database stall the request path.
func (w *Writer) Record(e Entry) {
	select {
	case w.entries <- e:
	default:
		metrics.LedgerDroppedTotal.Inc()
	}
}

func (w *Writer) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]Entry, 0, w.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.ins.InsertBatch(context.Background(), batch); err != nil {
			w.logger.Error("ledger: batch insert failed, rows dropped", "error", err, "rows", len(batch))
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-w.entries:
			batch = append(batch, e)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			// Drain whatever is already buffered before exiting -- "the
			// buffer is drained on shutdown" (README). This assumes
			// Close is only called after the HTTP server has already
			// stopped accepting requests, so no further Record calls
			// race with this drain; see cmd/gateway/main.go's shutdown
			// ordering.
			for {
				select {
				case e := <-w.entries:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Close stops the background loop and flushes whatever was buffered. It
// blocks until that flush completes or ctx is done, whichever comes
// first.
func (w *Writer) Close(ctx context.Context) error {
	w.cancel()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
