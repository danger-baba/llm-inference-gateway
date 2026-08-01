# 0008 — `/admin/*` has no authentication yet (known gap)

## Status

Accepted for Phase 4. This is a gap, not a design choice — it should not
survive to a real deployment.

## Context

Phase 4's scope, as specified, is "Admin endpoints: `POST /admin/keys`,
`DELETE /admin/keys/{id}`" with no accompanying admin authentication
scheme. The README doesn't specify one either — client authentication
(virtual keys) is fully specified, but nothing describes how an
operator's own request to issue or revoke a key is itself authenticated.

## Decision

Ship `/admin/keys` and `/admin/keys/{id}` with no authentication at all
for now, and name the gap explicitly here and in the README's Known
Limitations, rather than either (a) inventing an admin auth scheme not
asked for and presenting it as if it were specified, or (b) silently
shipping an open admin API with no visible warning.

## Why not just add something now

A real admin auth scheme has real design questions attached to it — a
separate static admin token via config/env? mTLS? An allowlist of admin
virtual keys with a role flag? Network-level restriction (bind admin
routes to a separate internal listener/port)? Each has different
operational trade-offs, and picking one silently, without it being asked
for, risks getting reviewed as if it were the README's design rather than
an improvisation. Naming the gap is more honest than a five-minute
placeholder that looks more finished than it is.

## Consequences

**Right now, anyone who can reach this gateway's HTTP port can issue
themselves a virtual key for any team, or revoke anyone else's.** This is
fine for local development and for this build's own gates, and is not
fine for any shared or internet-reachable deployment. Before that:

- Add this to the README's Known Limitations (done, see below).
- The most likely fix is a separate static admin credential (e.g. an
  `admin_token_env` config field checked via constant-time comparison on
  `/admin/*`), since it's the smallest change that closes the hole
  without inventing a second identity system.
