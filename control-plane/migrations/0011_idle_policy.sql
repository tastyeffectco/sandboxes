-- 0011 — per-sandbox idle policy (github.com/tastyeffectco/sandboxd/issues/11).
--
-- idle_policy controls how the idle reaper treats this sandbox:
--   'sleep'     — default: idle-stop after the global threshold, wake-on-request.
--   'always_on' — never idle-stopped; the sandbox keeps running indefinitely.
--
-- The DEFAULT is 'sleep' so all existing rows inherit the previous behaviour
-- with no data migration required.

ALTER TABLE sandbox ADD COLUMN idle_policy TEXT NOT NULL DEFAULT 'sleep';
