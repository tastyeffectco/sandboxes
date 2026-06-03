-- 0001 — initial schema for sandboxd.
--
-- The `sandbox` table is the source of truth for the lifecycle of a
-- sandbox: every running container with name prefix `s-` SHOULD have
-- exactly one row here. The reconciler converges Docker to this table;
-- it never adopts orphans (containers without rows) automatically —
-- see internal/reconcile/.
--
-- The `error_message` column lands in 0001 (not a later migration) so
-- failed-create paths and reconciler decisions have somewhere to write
-- their diagnosis from day one.
--
-- NOTE: the `migration` bookkeeping table is created by the migration
-- runner itself (internal/store/migrate.go) via `CREATE TABLE IF NOT
-- EXISTS migration (...)` before any *.sql file is applied. It is NOT
-- in this file. The original Phase 4 draft had a `CREATE TABLE
-- migration` block here, which collided with the runner's own creation
-- on first start; see ops/implementation/phase-4-report.md Issue #1.

PRAGMA foreign_keys = ON;

CREATE TABLE sandbox (
    id              TEXT PRIMARY KEY,           -- ULID
    status          TEXT NOT NULL,              -- creating | running | stopped | error
    image           TEXT NOT NULL,              -- e.g. sandbox-base:0.2.0
    workspace_img   TEXT NOT NULL,              -- /var/lib/sandboxed/workspaces/<id>.img
    workspace_mnt   TEXT NOT NULL,              -- /var/lib/sandboxed/workspaces/<id>.mnt
    container_id    TEXT,                       -- short docker id; NULL while creating
    cgroup_path     TEXT,                       -- relative path under /sys/fs/cgroup; NULL while creating
    memory_high     TEXT NOT NULL DEFAULT '4G',
    error_message   TEXT,                       -- last error from create/reconcile; NULL when healthy
    created_at      INTEGER NOT NULL,           -- unix seconds
    updated_at      INTEGER NOT NULL            -- unix seconds
);

CREATE INDEX sandbox_status_idx ON sandbox(status);

CREATE TABLE sandbox_port (
    sandbox_id TEXT NOT NULL,
    port       INTEGER NOT NULL,
    PRIMARY KEY (sandbox_id, port),
    FOREIGN KEY (sandbox_id) REFERENCES sandbox(id) ON DELETE CASCADE
);
