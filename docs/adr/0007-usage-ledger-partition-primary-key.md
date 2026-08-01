# 0007 — `usage_ledger`'s primary key is `(id, created_at)`, not `id`

## Status

Accepted (Phase 4).

## Context

The README's DDL declares `usage_ledger` as `PARTITION BY RANGE
(created_at)` with `id BIGSERIAL PRIMARY KEY`. PostgreSQL does not allow
this: every unique constraint (including a primary key) on a partitioned
table must include the full partition key as a subset of its columns,
because uniqueness on a partitioned table is enforced per-partition, not
globally — a partition boundary offers no way to check `id` uniqueness
against rows living in a different partition. Running the README's DDL
literally against real PostgreSQL fails at `CREATE TABLE` with exactly
this error.

## Decision

The primary key is `(id, created_at)`. `id` remains a `BIGSERIAL` and is
still what every foreign-key-shaped reference elsewhere in the codebase
means by "the ledger row's identity" — the extra `created_at` in the key
is a PostgreSQL implementation requirement, not a change to the table's
actual identity semantics.

## Alternatives considered

- **Drop partitioning, keep `id` as the sole primary key.** Rejected: the
  README is explicit that partitioning is the point — "Monthly partitions
  keep the hot partition small and make retention a `DROP TABLE` instead
  of a long-running `DELETE`" — and dropping it to avoid a DDL error would
  quietly abandon the actual design goal.
- **Use a global `UNIQUE` index instead of a `PRIMARY KEY`.** Not
  possible either: PostgreSQL applies the same partition-key-inclusion
  rule to any unique index on a partitioned table, not just primary keys.

## Consequences

Nothing that references `usage_ledger.id` needs to change — no other
table has a foreign key into the ledger (it's a leaf, append-only table),
so the composite key is invisible outside this migration. A `DEFAULT`
partition (`usage_ledger_default`) is created alongside the parent table
so every `INSERT` is valid immediately, regardless of when this migration
runs relative to any specific month; Phase 9 should add real monthly
partitions when the ledger writer lands, so the default partition stays
empty in normal operation.
