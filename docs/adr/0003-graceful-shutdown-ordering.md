# 0003 — Graceful shutdown ordering

## Status

Accepted (Phase 1).

## Context

The README's concurrency model specifies: "stop accepting, drain in-flight
requests up to a timeout, flush the ledger buffer, persist the HNSW index,
close pools." The ledger and HNSW steps don't exist until later phases, but
the accept/drain/close ordering needs to be right now, because retrofitting
correct shutdown semantics after request-handling logic already assumes a
particular lifecycle is a much bigger change than getting it right first.

## Decision

- `server.New` binds the TCP listener immediately (not lazily inside
  `Run`), so a "port already in use" error surfaces at construction time.
- `server.Run(ctx)` serves until `ctx` is cancelled (the caller wires this
  to `signal.NotifyContext` for SIGINT/SIGTERM), then calls
  `http.Server.Shutdown` with a bounded timeout derived from
  `server.shutdown_timeout`. `Shutdown` stops accepting new connections
  immediately and waits for in-flight handlers to return, up to that
  timeout.
- If the timeout elapses before in-flight requests finish, `Shutdown`
  returns the context's deadline error, which `Run` surfaces to `main`,
  which exits non-zero. The gateway does not force-kill stuck handlers
  itself — that's what the timeout and the orchestrator's own kill signal
  are for.
- Closing Redis/Postgres pools happens in `main`, after `srv.Run` returns,
  not inside the `server` package. `internal/server` only knows about
  `Pinger`, not about `*redis.Client` or `*pgxpool.Pool`, so it has nothing
  to close directly; ownership of those pools' lifecycle stays with the
  code that created them.

## Alternatives considered

- **`server` package owns and closes the pools itself.** Rejected: it
  would require importing the Redis/Postgres drivers into a package whose
  only real job is HTTP lifecycle, and it would make the package harder to
  unit test without a live Redis/Postgres.
- **Force-close in-flight connections on timeout instead of returning an
  error.** Rejected: silently dropping a request that might have almost
  finished is worse than a loud non-zero exit the orchestrator can alert on.

## Consequences

Phase 9's ledger buffer and Phase 7's HNSW persistence will each need their
own explicit flush/persist step sequenced between `srv.Run` returning and
`main` returning — this ADR only locks in the HTTP-layer half of the
sequence described in the README.
