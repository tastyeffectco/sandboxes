-- Agent configuration — single global row. Read by sandboxd at boot
-- into an atomic.Pointer cache; mutated by PUT /v1/agent-config which
-- bumps `version` so the per-task rewrite path knows to refresh each
-- sandbox's on-workspace files exactly once after a change.
--
-- Stage 1: global only. Stage 2 will add agent_config_sandbox for
-- per-sandbox overrides; the same Manager handles the merge.

CREATE TABLE agent_config_default (
    id          INTEGER PRIMARY KEY CHECK (id = 1),  -- single-row table
    model       TEXT    NOT NULL,
    agents_md   TEXT    NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    updated_at  INTEGER NOT NULL
);
