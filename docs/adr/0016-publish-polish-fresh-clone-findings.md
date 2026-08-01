# 0016 — Publish polish: what a real fresh clone found

## Status

Accepted (Phase 11).

## Context

Phase 11's gate is specific: don't just read the Getting Started section
and believe it, actually clone the repository into a new directory and
run what's written there. Doing that literally (not just building in the
long-lived working directory this whole project was developed in) found
one real, worth-fixing problem.

## Decisions

**Added `.gitattributes` (`* text=auto eol=lf`) after a fresh clone
showed every `.go` file failing `gofmt -l .`.** This machine's `git config
core.autocrlf` is `true` — the common default after a standard Git for
Windows install — so a plain `git clone` checked every text file out with
CRLF line endings. `gofmt` normalizes to LF and reported every single
file in the repository as needing reformatting, even though nothing was
actually wrong with any of them; it was purely a checkout-time line-ending
conversion. This would hit any Windows contributor with the default Git
configuration, not just this environment's session — CONTRIBUTING.md
tells a new contributor to run `gofmt -l .` and expect nothing printed,
and that would have been false advice without this fix. Re-cloned after
adding the attribute and re-normalizing: line endings check out as LF, and
`gofmt -l .`, `go build ./...`, `go vet ./...`, `go test ./...`, and
`golangci-lint run` all pass cleanly from the fresh clone.

**What was and wasn't verified from that fresh clone.** Verified for
real: `go build ./...`, `go vet ./...`, `go test ./...` (Redis/ONNX-backed
tests self-skip without those dependencies present, exactly as designed),
`gofmt -l .`, `golangci-lint run`, and `docker compose config` (validates
the compose file's syntax without needing the daemon). Not verified in
this environment: `docker compose up -d`, `make migrate`, `make seed`,
and the curl/Grafana walkthrough all need a running Docker daemon, which
has been unavailable in this development environment since Phase 1 (the
same admin-rights/Docker Desktop service blocker noted there) — not
something Phase 11 newly discovered, but worth restating here since this
is the phase whose entire job is fresh-clone verification. The
`docker-compose.yml`, `Dockerfile`, and migration SQL have all been
reviewed and are internally consistent, but "internally consistent" is
not the same claim as "verified by actually running them," and this ADR
says so rather than letting the distinction blur.

## Consequences

- Any future contributor cloning this repo on Windows with default Git
  settings gets LF-normalized files automatically; the `gofmt`-fails-on-
  every-file problem this ADR describes cannot recur for them.
- The Docker-daemon-dependent portion of Getting Started remains
  genuinely unverified by this project's own development process, not
  just by this one session — a real gap, disclosed rather than implied
  away by the rest of this phase's verification having gone smoothly.
