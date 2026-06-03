-- 0002 — activity columns for the idle reaper, pressure reaper, and wake path.
--
-- Phase 5 adds three columns to `sandbox` so the reapers can decide
-- which rows are idle, when to revive a stopped row, and which rows
-- the operator has explicitly pinned:
--
--   last_active_at  — Unix seconds; bumped by the access-log tailer,
--                     open-connection poller (if available), exec
--                     enter/exit, and wake handler.
--   stopped_at      — Unix seconds when `docker stop` succeeded;
--                     NULL when running. Used by audit-trail logs.
--   keepalive_until — Unix seconds; idle reaper MUST NOT stop a row
--                     while `keepalive_until > now`. Clamped to
--                     24 hours in the future (the API enforces the
--                     ceiling; the DB just stores).
--
-- Index speeds up the idle-candidate query
-- (`WHERE status='running' AND last_active_at < ?` ORDER BY
-- last_active_at ASC).
--
-- IMPORTANT: the `migration` bookkeeping table is OWNED by the
-- runner (internal/store/migrate.go via `IF NOT EXISTS`). Do NOT
-- include `CREATE TABLE migration` here — that was the Phase 4
-- Issue #1 own-goal. This file ALTERs the existing schema only.

ALTER TABLE sandbox ADD COLUMN last_active_at  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox ADD COLUMN stopped_at      INTEGER;
ALTER TABLE sandbox ADD COLUMN keepalive_until INTEGER;

CREATE INDEX sandbox_last_active_idx ON sandbox(status, last_active_at);
