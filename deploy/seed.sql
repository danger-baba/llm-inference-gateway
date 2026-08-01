-- Demo org/team for `make seed`. Fixed UUIDs so re-running is a no-op.
-- There's no admin endpoint for orgs/teams yet (only virtual_keys, per
-- Phase 4's scope), so this is plain SQL rather than an API call.
INSERT INTO orgs (id, name, tpm_limit)
VALUES ('00000000-0000-0000-0000-000000000001', 'demo-org', 200000)
ON CONFLICT (id) DO NOTHING;

INSERT INTO teams (id, org_id, name, tpm_limit)
VALUES ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000001', 'demo-team', 200000)
ON CONFLICT (id) DO NOTHING;
