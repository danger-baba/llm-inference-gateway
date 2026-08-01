# 0002 — Added `redis:` and `postgres:` config sections

## Status

Accepted (Phase 1).

## Context

`/readyz` must check Redis and Postgres reachability (README, API surface
table), but the README's illustrative `config.yaml` snippet has no top-level
`redis:` or `postgres:` section — it shows `server`, `providers`,
`model_aliases`, `fallback_chains`, `breaker`, `retry`, `cache`,
`rate_limit`, `batching`, `observability` only. Something has to carry the
connection details, and Phase 1's own gate (`make up` + a live `/readyz`)
cannot pass without them.

## Decision

Add two sections not present in the README's example, following the same
pattern the README already uses for provider secrets (`api_key_env`: a
reference to an environment variable, never a plaintext secret in the
YAML):

```yaml
redis:
  addr: "redis:6379"
  password_env: REDIS_PASSWORD   # optional; empty means no auth
  db: 0
  dial_timeout: 2s

postgres:
  dsn_env: POSTGRES_DSN          # required; DSN itself lives in the environment
  ping_timeout: 2s
```

`dial_timeout` and `ping_timeout` are also new: they bound each dependency
check in `/readyz` independently, so one slow dependency can't stall the
other or block past what a health-check caller is willing to wait.

## Alternatives considered

- **Single `POSTGRES_DSN`/`REDIS_ADDR` environment variables with no YAML
  section at all.** Rejected: inconsistent with how every other connection
  detail in this project (provider `base_url`, `timeout`, etc.) lives in
  YAML with only secrets carved out to the environment.
- **Guess that `redis`/`postgres` config was meant to be inferred from
  `providers`.** Rejected: providers are LLM backends, not infrastructure
  dependencies; conflating them would be a bigger schema distortion than
  adding two small sections.

## Consequences

The README's config example is now slightly incomplete relative to what
actually ships; Phase 11's README pass should add these two sections to the
documented example so the two artifacts stay in sync.
