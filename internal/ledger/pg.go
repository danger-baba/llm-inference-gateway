package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// usageLedgerColumns must stay in the same order as the []any built in
// InsertBatch below -- pgx.CopyFrom matches by position, not by name.
var usageLedgerColumns = []string{
	"request_id", "org_id", "team_id", "virtual_key_id", "provider", "model",
	"prompt_tokens", "completion_tokens", "tokens_saved", "cost_usd",
	"cache_tier", "attempts", "status_code", "latency_ms",
}

// PGInserter is the real Inserter, backed by a COPY into usage_ledger.
// COPY, not a multi-row INSERT, because this is exactly its intended use
// case -- appending many complete rows with no conflict handling and no
// need for RETURNING -- and it's meaningfully faster under the batch
// sizes a busy gateway will actually produce.
type PGInserter struct {
	pool *pgxpool.Pool
}

func NewPGInserter(pool *pgxpool.Pool) *PGInserter {
	return &PGInserter{pool: pool}
}

func (p *PGInserter) InsertBatch(ctx context.Context, entries []Entry) error {
	rows := make([][]any, len(entries))
	for i, e := range entries {
		rows[i] = []any{
			e.RequestID, e.OrgID, e.TeamID, e.VirtualKeyID, e.Provider, e.Model,
			e.PromptTokens, e.CompletionTokens, e.TokensSaved, e.CostUSD,
			e.CacheTier, e.Attempts, e.StatusCode, e.LatencyMS,
		}
	}
	_, err := p.pool.CopyFrom(ctx, pgx.Identifier{"usage_ledger"}, usageLedgerColumns, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("ledger: copy batch of %d rows into usage_ledger: %w", len(entries), err)
	}
	return nil
}
