-- Phase 6 — container_ip column.
--
-- container_ip is the sandbox container's bridge IP, populated by
-- sandboxd after `docker run` succeeds and used as the join key when
-- correlating kernel egress-log lines (`SRC=...`) back to a sandbox.
-- NULL while the sandbox is stopped; refreshed on wake.
--
-- The partial index keeps lookups fast without paying the storage
-- cost of indexing rows where the column is NULL (most stopped rows).
-- See roadmap/phase-6-hardening-and-egress.md §1.

ALTER TABLE sandbox ADD COLUMN container_ip TEXT;

CREATE INDEX sandbox_container_ip_idx
    ON sandbox(container_ip)
    WHERE container_ip IS NOT NULL;
