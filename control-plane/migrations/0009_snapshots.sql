-- 0009 — snapshots-as-templates (ops/design/snapshots-as-templates.md).
--
-- A snapshot is a reusable, frozen copy of a sandbox's workspace .img,
-- stored raw in /var/lib/sandboxed/library/<id>.img and cloned
-- into new sandboxes via the existing ProvisionFromTemplate path.
--
-- Scoped to the API tenant (owner_token = auth.Actor.Name), NOT to the
-- untrusted external user_id. base_image is recorded for provenance but
-- not pinned on spin-up. visibility + format columns are reserved so
-- public-sharing and compression are future slices with no schema change.
--
-- See migrations/0005 — do NOT create the `migration` bookkeeping table
-- here; the runner (internal/store/migrate.go) owns it.

CREATE TABLE snapshot (
    id                  TEXT PRIMARY KEY,                 -- ULID
    name                TEXT NOT NULL,
    owner_token         TEXT NOT NULL,                    -- auth.Actor.Name (tenant boundary)
    source_sandbox_id   TEXT,                             -- provenance
    created_by_user_id  TEXT,                             -- untrusted passthrough; provenance only
    base_image          TEXT NOT NULL,                    -- captured sb.image; recorded, not pinned
    visibility          TEXT NOT NULL DEFAULT 'private',  -- 'private' only in v1
    format              TEXT NOT NULL DEFAULT 'raw',      -- 'raw' | (future) 'zst'
    status              TEXT NOT NULL,                    -- ready | error
    image_path          TEXT NOT NULL,                    -- /var/lib/sandboxed/library/<id>.img
    size_bytes          INTEGER,
    error_message       TEXT,
    created_at          INTEGER NOT NULL
);
CREATE INDEX snapshot_owner_idx ON snapshot(owner_token);
