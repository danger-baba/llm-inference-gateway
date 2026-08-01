# 0001 — Strict, whole-schema config validation

## Status

Accepted (Phase 1).

## Context

The README requires that config loading reject unknown YAML fields rather
than silently ignoring them, and that malformed config produce clear errors.
A gateway that starts successfully on a config with a typo'd key (e.g.
`read_timout` instead of `read_timeout`) is worse than one that refuses to
start, because the typo'd field silently falls back to a zero value and the
operator only finds out at 2am.

## Decision

- The full `Config` struct mirrors every section in the README's example
  YAML, decoded with `yaml.v3`'s `Decoder.KnownFields(true)`, which rejects
  any field not present in the struct — recursively, at every nesting level.
- `Duration` is a custom type wrapping `time.Duration` with its own
  `UnmarshalYAML`, since `yaml.v3` has no native support for parsing
  strings like `"30s"` into a duration.
- `Validate()` collects every violation via `errors.Join` instead of
  returning on the first failure, so a broken config is fixed in one pass
  instead of N round trips.
- Only the sections a shipped phase actually reads (`server`, `redis`,
  `postgres`, `observability.log_level`) get range/required-field
  validation right now. Sections for unbuilt phases (`providers`,
  `breaker`, `retry`, `cache`, `rate_limit`, `batching`) are parsed and
  still reject unknown fields, but their numeric ranges (e.g.
  `error_rate_threshold` must be in `[0,1]`) are validated by the phase
  that first consumes them.

## Alternatives considered

- **Validate everything now, including unused sections.** Rejected: it
  would force every local dev config to already populate values nobody
  reads, and the "right" range for e.g. `breaker.cooldown_max` is easier to
  get right once the breaker package that enforces it exists.
- **Loose YAML decoding (`map[string]interface{}` + manual field lookups).**
  Rejected: throws away compile-time field checking and makes "unknown
  field" detection something to hand-roll instead of getting for free from
  the library.

## Consequences

Adding a field to any section in a later phase is a one-line struct change;
forgetting to update a config file that still uses the old field name
becomes a startup error instead of silent misbehaviour. The trade-off is
that a config file must be updated in lockstep with the phase that changes
the schema — there is no fallback/default path for a stale field name.
