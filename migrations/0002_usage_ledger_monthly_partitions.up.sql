-- Real monthly partitions for usage_ledger, so usage_ledger_default (the
-- catch-all partition from migration 0001) stays empty in practice
-- rather than becoming an unbounded, unindexed dumping ground.
--
-- This covers calendar year 2026. A static migration can't keep pace
-- with time forever -- a production deployment needs a scheduled job
-- (pg_partman, or a small cron invoking "create next month's partition")
-- to keep adding partitions ahead of need. Tracked as a roadmap item;
-- see docs/adr/0014.
CREATE TABLE usage_ledger_2026_01 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE usage_ledger_2026_02 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE usage_ledger_2026_03 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
CREATE TABLE usage_ledger_2026_04 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01');
CREATE TABLE usage_ledger_2026_05 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE usage_ledger_2026_06 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
CREATE TABLE usage_ledger_2026_07 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');
CREATE TABLE usage_ledger_2026_08 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE usage_ledger_2026_09 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE usage_ledger_2026_10 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE usage_ledger_2026_11 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
CREATE TABLE usage_ledger_2026_12 PARTITION OF usage_ledger
    FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');
