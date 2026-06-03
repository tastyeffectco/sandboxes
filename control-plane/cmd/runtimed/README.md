# runtimed — in-sandbox supervisor

`runtimed` runs **inside every sandbox container** as its main process.
Its job (per `ops/design/v1-external-api.md` §4 and the runtimed
contract) is to supervise the app's dev server and run coding tasks —
so the preview survives idle→wake and task execution has a stable
owner. The host control plane `sandboxd` talks to it over a Unix
domain socket; `runtimed` itself is never publicly reachable.

This is built **incrementally**. This document tracks what is actually
implemented.

## Implemented — slice 1 (dev-server supervision)

- **Supervisor process.** Long-lived; intended to run as the container
  main process under `tini` (`docker run --init`). Clean shutdown on
  SIGTERM/SIGINT.
- **Dev-server supervision.** Starts the Vite dev server
  (`bash -lc "pnpm dev"` in the app dir, its own process group);
  restarts it with exponential backoff on unexpected exit; after
  `maxFastFails` consecutive fast failures it stops restarting and
  reports the preview as `down` rather than crash-looping.
- **Health probing.** Polls the dev server's HTTP port and derives
  `preview.status` (`down` / `starting` / `ready`).
- **Control surface.** `GET /status` — `runtime.Status` JSON — served
  over HTTP/1.1 on a Unix domain socket at
  `/home/sandbox/.runtimed/sock`. The socket is on the durable
  workspace, so `sandboxd` reaches the same inode on the host at
  `<workspaces>/<id>.mnt/.runtimed/sock`. No network port; no
  cross-tenant reachability.
- **`internal/runtime`** — the shared protocol types and the
  `sandboxd`-side `runtime.Client` (the integration seam).

## Implemented — slice 2 (coding tasks, OpenCode)

- **Task subsystem.** `POST /tasks`, `GET /tasks/{id}/events`,
  `POST /tasks/{id}/cancel`. Exactly **one active task at a time** per
  sandbox — a concurrent `POST /tasks` returns `409 task_in_progress`.
- **Task lifecycle.** Per task: a pre-task git checkpoint (the app dir
  is `git init`-ed on first use) → run the agent → authoritative
  `files_changed` from `git diff` against the checkpoint → a post-task
  build check (`pnpm build`) → the canonical `runtime.TaskResult`.
- **OpenCode adapter** — drives `opencode run --format json
  --dangerously-skip-permissions`, parsing its JSON event stream into
  canonical `message` events. The `agent` interface is the adapter
  boundary; Claude Code / Codex are deferred.
- **Events.** Monotonic event stream — `status` / `message` / `build`
  / `done` — appended to `.runtimed/tasks/<id>/events.jsonl` and
  streamed live (newline-delimited JSON, resumable via `?since=`).
  `done` is the single terminal event and carries the result.
- **Persistence + interrupt recovery.** Each task has
  `.runtimed/tasks/<id>/` with `events.jsonl`, `result.json`,
  `agent.log`. On boot, a task with an event log but no `result.json`
  (interrupted by a stop/crash) is finalized as `failed` —
  never resumed.
- **Cancellation.** Cancel kills the agent's process group; the task
  finalizes as `cancelled`. Timeout is runtimed-initiated cancellation
  (`failed` / `agent_timeout`).
- **`active_task`** is reported in `GET /status`.

## Integrated — slice 3 (base image + sandboxd)

- `runtimed` is the `sandbox-base` image's `CMD`, under the existing
  `tini` `ENTRYPOINT` — the container's main process. Built into the
  image by `image/build.sh`; image tag `0.3.0`.
- `sandboxd` creates sandboxes on `sandbox-base:0.3.0`, so every new
  sandbox boots `runtimed` automatically — no `docker exec` dev server.
- `GET /sandbox/{id}` surfaces the in-sandbox `runtime` block (preview
  state + active task) via `runtime.Client` over the UDS.
- Validated on the real path (2026-05-16): a sandbox created through
  `POST /sandbox` ran `runtimed`, reached preview `ready` in ~3 s, ran
  an OpenCode task to `succeeded` / `build_ok`, and the preview
  survived a stop→wake cycle (recovered `ready` in ~3 s).

## Public API — slice 4 (the /v1 surface)

The narrow public `/v1` API (`ops/design/v1-external-api.md`) is
implemented in `control-plane/internal/api/v1*.go` — a thin
translation layer over the proven internal machinery and `runtimed`:

- Sandbox `create` / `get` / `stop` / `delete` — create is idempotent
  per project; `delete` is a full destroy (workspace removed).
- Task `submit` / `get` / `events` (SSE) / `cancel` — submit and
  cancel proxy to `runtime.Client`; `events` reframes the runtimed
  NDJSON stream as SSE, resumable via `Last-Event-ID`.
- File `list` / `content` / `export` — served by `sandboxd` directly
  from the host-side workspace mount.

Validated end-to-end through the real integrated system (2026-05-16):
create → preview `ready` → an OpenCode task to `succeeded` /
`build_ok` → file read → `export` zip → `stop` → `delete`.

## Durability — slice 5 (wake-on-submit + task retention)

- **Wake-on-task-submit** — `POST /v1/.../tasks` to a stopped sandbox
  wakes it first (delegates to the internal wake path), then submits.
- **Durable task store** — `sandboxd` records every task in SQLite
  (migration `0005_tasks.sql`); a background watcher captures the
  canonical result from runtimed's terminal event into the `task`
  table, independent of any client SSE connection.
- **Task get is sandbox-independent** — `GET /v1/.../tasks/{id}` reads
  SQLite, so it works after the sandbox is stopped *and* after it is
  destroyed. The result is stored outside the sandbox workspace.

Validated (2026-05-16): `GET` returns the task result after `stop`
and after `delete`; a task submitted to a stopped sandbox wakes it
and runs to `succeeded`.

## Task-lifecycle reliability — slice 6

- **Boot-time task reconciliation** — on startup `sandboxd` finalizes
  any task left `running` by a previous run: from runtimed's
  `result.json` if present, else by re-attaching a watcher if the
  sandbox is still up, else as a clean `failed` /
  `sandbox_unavailable`. No new terminal state is introduced.
- **Active-task idle-reap suppression** — the idle reaper skips a
  sandbox that has a running task; reaping resumes once the task ends.
- **Private-sandbox wake-on-submit** — `gatePrivateWake` is skipped
  for `service`/`operator`-authenticated callers, so wake-on-submit
  works for private sandboxes; the end-user preview-URL wake path is
  unchanged.

Validated (2026-05-16): a task survives a `sandboxd` restart mid-run
(watcher re-attached); a sandbox with a running task is not
idle-reaped past the threshold and is reaped once the task ends; a
stopped private sandbox wakes on task submit.

## Configuration (environment)

| Variable | Default |
|---|---|
| `RUNTIMED_APP_DIR` | `/home/sandbox/workspace/app` |
| `RUNTIMED_DIR` | `/home/sandbox/.runtimed` |
| `RUNTIMED_SOCKET` | `<RUNTIMED_DIR>/sock` |
| `RUNTIMED_DEV_CMD` | `pnpm dev` |
| `RUNTIMED_PREVIEW_PORT` | `3000` |
| `RUNTIMED_PROBE_INTERVAL_SECONDS` | `3` |

## Build

```sh
CGO_ENABLED=0 go build -o runtimed ./cmd/runtimed
```

Pure Go, statically linked — runs in the Debian-slim sandbox image
with no extra runtime dependencies.

## Validation procedure

Validates `runtimed` end-to-end against a clone of the golden snapshot
without touching the live platform (throwaway container + image clone):

1. `CGO_ENABLED=0 go build -o /tmp/runtimed ./cmd/runtimed`.
2. Clone a golden image and loopback-mount it:
   `cp --reflink=auto templates/react-standard-<ver>.img /tmp/rt.img`,
   `mount -o loop /tmp/rt.img /tmp/rt.mnt`.
3. Run `runtimed` in a `sandbox-base` container:
   `docker run -d --init --user sandbox -v /tmp/rt.mnt:/home/sandbox
   -v /tmp/runtimed:/usr/local/bin/runtimed:ro
   --entrypoint /usr/local/bin/runtimed sandbox-base:<ver>`.
4. Poll `GET /status` over the UDS from the host:
   `curl --unix-socket /tmp/rt.mnt/.runtimed/sock http://runtimed/status`
   — expect `preview.status` to reach `ready`.
5. Confirm the dev server serves: `docker exec <c> curl localhost:3000`
   → HTTP 200.
6. Supervision: `docker exec <c> sh -c "kill -- -<pid>"` (the
   dev-server process group) — expect `restarts` to increment and
   `preview.status` to recover.
7. Clean shutdown: `docker stop <c>` — expect a fast, exit-code-0
   shutdown (not the 10 s SIGKILL fallback).
8. Teardown: `docker rm -f`, `umount`, `rm` the clone.

For slice 2, additionally: `POST /tasks` a file-creating prompt,
stream `GET /tasks/{id}/events` to the terminal `done`, and check
`.runtimed/tasks/<id>/result.json` (`status`, `files_changed`,
`build_ok`, `checkpoint_id`); a mid-run `POST /tasks/{id}/cancel`
should finalize the task as `cancelled`.

Last run (2026-05-16, golden `react-standard-0.1.0`): `ready` in ~3 s;
supervision restart on dev-server kill; `docker stop` clean in 131 ms.
An OpenCode task created `src/components/Greeting.tsx` and finalized
`succeeded` / `build_ok: true` in ~19 s; a cancelled task finalized
`cancelled` in 32 ms.

## Intentionally NOT implemented yet

- **Task event-log retention past destroy** — only the canonical
  *result* is retained (in SQLite); the full event *log* lives with
  runtimed in the workspace and is gone once the sandbox is destroyed.
  By design (`ops/design/v1-external-api.md` §4.5).

Other deferred items:

- **Claude Code / Codex adapters** — the `agent` interface is the
  seam; only OpenCode is implemented.
- **Provider-derived `tool` / `file_change` events** — only `message`
  events are surfaced from the agent; `files_changed` is computed
  authoritatively from git regardless.
- **Dev-server restart on dependency changes** — a task that changes
  `package.json` does not yet trigger a dev-server restart.
