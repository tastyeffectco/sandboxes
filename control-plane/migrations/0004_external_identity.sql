-- 0004 — external identity, workspace ownership, audit log (Phase 8).
--
-- Phase 8 makes sandboxd an infrastructure subsystem of an upstream
-- application. The upstream owns end-user identity; sandboxd records
-- the upstream's opaque identifiers as pure passthrough strings.
--
--   external_user_id      — required on every Phase-8-or-later create;
--                           NULL only on legacy (pre-Phase-8) rows
--                           until `sandboxd backfill-legacy` runs.
--   external_project_id   — optional upstream project key.
--   external_workspace_id — optional upstream workspace key.
--   visibility            — 'public' (URL is the only gate, Phase 3
--                           behaviour) or 'private' (forward-auth
--                           gated). DEFAULT 'public' so every legacy
--                           row keeps its pre-Phase-8 behaviour.
--
-- `workspace_owner` is the DURABLE binding from sandbox_id to upstream
-- identity. It deliberately survives `DELETE /sandbox/{id}` so the
-- id-reuse path (Phase 4) and the snapshot endpoints (Phase 7) can
-- still authorize a reattach. Per-sandbox purge (Phase 8 step 6) is
-- the only thing that removes a workspace_owner row.
--
-- `audit_log` is append-only. One row per privileged action. v1 query
-- access is direct `sqlite3` from the host (roadmap §12); no read API.
--
-- IMPORTANT: the `migration` bookkeeping table is OWNED by the runner
-- (internal/store/migrate.go via `IF NOT EXISTS`). Do NOT create it
-- here — that was the Phase 4 Issue #1 own-goal. This file ALTERs the
-- existing schema and adds new tables only.

PRAGMA foreign_keys = ON;

ALTER TABLE sandbox ADD COLUMN external_user_id      TEXT;
ALTER TABLE sandbox ADD COLUMN external_project_id   TEXT;
ALTER TABLE sandbox ADD COLUMN external_workspace_id TEXT;
ALTER TABLE sandbox ADD COLUMN visibility            TEXT NOT NULL DEFAULT 'public';

CREATE INDEX sandbox_external_user_idx    ON sandbox(external_user_id);
CREATE INDEX sandbox_external_project_idx ON sandbox(external_project_id);

CREATE TABLE workspace_owner (
    sandbox_id            TEXT PRIMARY KEY,
    external_user_id      TEXT NOT NULL,
    external_project_id   TEXT,
    external_workspace_id TEXT,
    created_at            INTEGER NOT NULL
);
CREATE INDEX workspace_owner_external_user_idx    ON workspace_owner(external_user_id);
CREATE INDEX workspace_owner_external_project_idx ON workspace_owner(external_project_id);

CREATE TABLE audit_log (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    at               INTEGER NOT NULL,
    actor_kind       TEXT NOT NULL,   -- 'service' | 'operator' | 'system' | 'unknown'
    actor_name       TEXT,            -- token name, or operator note
    actor_ip         TEXT,
    external_user_id TEXT,            -- upstream user this affects, if any
    action           TEXT NOT NULL,
    target           TEXT,            -- sandbox_id / external_user_id / ...
    detail           TEXT             -- JSON
);
CREATE INDEX audit_log_at_idx            ON audit_log(at);
CREATE INDEX audit_log_action_idx        ON audit_log(action);
CREATE INDEX audit_log_external_user_idx ON audit_log(external_user_id);
