-- 0005 — coding-task / result retention (runtimed slice 5).
--
-- sandboxd records one row per coding task so a task's canonical
-- result survives the sandbox being stopped or destroyed. Per
-- ops/design/v1-external-api.md §4: the result lives here (SQLite,
-- outside any sandbox workspace); the full event LOG stays with
-- runtimed in the workspace and is NOT retained past destroy.
--
-- result_json is the marshalled runtime.TaskResult; NULL while the
-- task is still running. A background watcher in sandboxd fills it in
-- from runtimed's terminal `done` event.
--
-- See migrations/0004 — do NOT create the `migration` bookkeeping
-- table here; the runner (internal/store/migrate.go) owns it.

PRAGMA foreign_keys = ON;

CREATE TABLE task (
    task_id             TEXT PRIMARY KEY,
    sandbox_id          TEXT NOT NULL,
    external_user_id    TEXT,
    external_project_id TEXT,
    agent               TEXT NOT NULL,
    prompt              TEXT NOT NULL,
    status              TEXT NOT NULL,   -- running | succeeded | failed | cancelled
    result_json         TEXT,            -- canonical runtime.TaskResult; NULL while running
    created_at          INTEGER NOT NULL,
    finished_at         INTEGER          -- NULL while running
);
CREATE INDEX task_sandbox_idx ON task(sandbox_id);
