package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Scope names which usage_ledger column an Aggregate query filters on --
// GET /admin/usage's "by scope" (README, API surface).
type Scope string

const (
	ScopeOrg  Scope = "org"
	ScopeTeam Scope = "team"
	ScopeKey  Scope = "key"
)

// scopeColumn is a closed whitelist, not a pass-through of the caller's
// own string: Aggregate interpolates this value directly into SQL, so it
// must never be able to come from request input.
var scopeColumn = map[Scope]string{
	ScopeOrg:  "org_id",
	ScopeTeam: "team_id",
	ScopeKey:  "virtual_key_id",
}

func ValidScope(s Scope) bool {
	_, ok := scopeColumn[s]
	return ok
}

// UsageAggregate is what GET /admin/usage reports for one scope, one ID,
// and one time window -- "ledger aggregates by scope and window" is the
// entirety of the README's spec for this endpoint, so the shape here is
// this project's own design, not a literal reading of anything written
// down. See docs/adr/0014.
type UsageAggregate struct {
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	TokensSaved      int64
	CostUSD          float64
	CacheHits        map[string]int64 // "none" | "exact" | "semantic" -> count
}

// Aggregator is the narrow interface handleUsage needs; *PGAggregator
// satisfies it against a real Postgres pool.
type Aggregator interface {
	Aggregate(ctx context.Context, scope Scope, id uuid.UUID, since, until time.Time) (UsageAggregate, error)
}

type PGAggregator struct {
	pool *pgxpool.Pool
}

func NewPGAggregator(pool *pgxpool.Pool) *PGAggregator {
	return &PGAggregator{pool: pool}
}

func (a *PGAggregator) Aggregate(ctx context.Context, scope Scope, id uuid.UUID, since, until time.Time) (UsageAggregate, error) {
	col, ok := scopeColumn[scope]
	if !ok {
		return UsageAggregate{}, fmt.Errorf("ledger: unknown scope %q", scope)
	}

	query := fmt.Sprintf(`
		SELECT
			count(*),
			coalesce(sum(prompt_tokens), 0),
			coalesce(sum(completion_tokens), 0),
			coalesce(sum(tokens_saved), 0),
			coalesce(sum(cost_usd), 0),
			count(*) FILTER (WHERE cache_tier = 'none'),
			count(*) FILTER (WHERE cache_tier = 'exact'),
			count(*) FILTER (WHERE cache_tier = 'semantic')
		FROM usage_ledger
		WHERE %s = $1 AND created_at >= $2 AND created_at < $3`, col)

	var agg UsageAggregate
	var none, exact, semantic int64
	err := a.pool.QueryRow(ctx, query, id, since, until).Scan(
		&agg.Requests, &agg.PromptTokens, &agg.CompletionTokens, &agg.TokensSaved, &agg.CostUSD,
		&none, &exact, &semantic,
	)
	if err != nil {
		return UsageAggregate{}, fmt.Errorf("ledger: aggregate query: %w", err)
	}
	agg.CacheHits = map[string]int64{"none": none, "exact": exact, "semantic": semantic}
	return agg, nil
}
