# AGENTS.md — operating sandboxed

A complete, self-contained runbook for an AI agent (or a human) to install, run,
use, and remove **sandboxed** with no outside knowledge. Commands are
copy-pasteable. Human-readable docs: [`README.md`](README.md),
[`ARCHITECTURE.md`](ARCHITECTURE.md).

## What this is
A single-host service that creates isolated Linux dev containers ("sandboxes"),
each with an HTTP/HTTPS preview URL for a dev server running inside it. It runs
entirely on Docker: a Go control plane (`sandboxd`) + Traefik, both in
containers; each sandbox is a sibling container. Sandboxes stop when idle and
wake on the next request. Workspaces persist on disk.

## Prerequisites
- Linux host with **Docker Engine** + the **Compose plugin** (`docker compose`).
- Ability to run Docker (either your user is in the `docker` group, or you have
  `sudo`; the scripts auto-detect and use `sudo` if needed).
- Assumes a standard Docker daemon. (If the daemon uses `userns-remap`, the
  shipped `--userns=host` defaults keep it working — see ARCHITECTURE.md.)

## Install
```bash
git clone https://github.com/tastyeffectco/sandboxes.git
cd sandboxes
./install.sh
```
`install.sh` is idempotent. It: checks Docker, copies `.env.example`→`.env`,
builds the base image (`sandboxed-base:1.0.0`) and the control plane, creates the
data dir, and runs `docker compose up -d`. The base-image build takes a few
minutes the first time, then caches.

Configuration lives in `.env` (all keys documented in `.env.example`). The two
you may change before installing:
- `HTTP_PORT` (default `80`) — set to e.g. `8088` if port 80 is taken; preview
  URLs then include it.
- `SANDBOXED_API_BIND` (default `127.0.0.1:9090`) — where the API is published.

Verify it's up:
```bash
curl -s http://127.0.0.1:9090/healthz   # -> ok
curl -s http://127.0.0.1:9090/readyz    # -> ready
```

## Core API
Base URL = `http://${SANDBOXED_API_BIND}` (default `http://127.0.0.1:9090`).
Auth is **off by default** (local). If you set `SANDBOXD_API_AUTH_DISABLED=false`
+ `SANDBOXD_API_TOKENS=name:secret`, add `-H "Authorization: Bearer secret"`.

| Method & path | Body | Purpose |
|---|---|---|
| `POST /sandbox` | `{"ports":[3000],"env":{"ANTHROPIC_API_KEY":"sk-..."}}` | create. `id` optional (ULID auto); `env` injects vars into the container (agent keys). Returns the row incl. `id`, `status` |
| `GET /sandboxes` | — | list all |
| `GET /sandbox/{id}` | — | get one (status, ports, …) |
| `POST /sandbox/{id}/exec` | `{"cmd":["bash","-lc","..."]}` | run a command (NON-interactive: no TTY/stdin) |
| `POST /sandbox/{id}/keepalive` | — | postpone the idle reaper |
| `POST /v1/sandboxes/{id}/stop` | — | stop now (frees RAM); wakes on next preview hit |
| `DELETE /sandbox/{id}` | — | destroy the container, KEEP the workspace |
| `POST /sandbox/{id}/purge` | — | destroy + DELETE the workspace |
| `PUT /v1/sandboxes/{id}/files` | `{"path":"...","content":"...","append":false}` | write a file into the workspace |
| `GET /v1/sandboxes/{id}/files` / `.../files/content?path=` | — | list / read workspace files |
| `POST /v1/sandboxes/{id}/tasks` | (agent task) | run a coding agent headlessly via `runtimed` |
| `GET /healthz`, `GET /readyz` | — | liveness / readiness |

## End-to-end example
```bash
API=http://127.0.0.1:9090

# 1. create a sandbox exposing port 3000
ID=$(curl -s -XPOST $API/sandbox -H 'content-type: application/json' \
       -d '{"ports":[3000]}' | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "sandbox=$ID"

# 2. start a dev server inside it
curl -s -XPOST $API/sandbox/$ID/exec -H 'content-type: application/json' \
  -d '{"cmd":["bash","-lc","cd ~/workspace && echo hello > index.html && python3 -m http.server 3000"]}'

# 3. open the preview (browsers resolve *.localhost to 127.0.0.1).
#    add :$HTTP_PORT if you changed it from 80.
curl -s -H "Host: s-$ID-3000.preview.localhost" http://127.0.0.1:${HTTP_PORT:-80}/

# 4. stop (idle) and wake-on-request
curl -s -XPOST $API/v1/sandboxes/$ID/stop
curl -s -H "Host: s-$ID-3000.preview.localhost" http://127.0.0.1:${HTTP_PORT:-80}/   # wakes it

# 5. destroy
curl -s -XPOST $API/sandbox/$ID/purge
```

## Running Claude Code / OpenCode inside a sandbox
Both CLIs (`claude`, `opencode`) are pre-installed in every sandbox; outbound
network is allowed. The clean path is to inject the key at **create** time so
both the tasks API and any shell see it:
```bash
# create with the key, then drive OpenCode headlessly via the tasks API:
ID=$(curl -s -XPOST $API/sandbox -H 'content-type: application/json' \
       -d '{"ports":[3000],"env":{"ANTHROPIC_API_KEY":"sk-ant-..."}}' \
       | sed -E 's/.*"id":"([^"]+)".*/\1/')
curl -s -XPOST $API/v1/sandboxes/$ID/tasks -H 'content-type: application/json' \
  -d '{"prompt":"build a Vite todo app and run it on port 3000","agent":"opencode"}'
# stream progress: curl -N $API/v1/sandboxes/$ID/tasks/<taskId>/events
```
Other ways to supply the key:
```bash
# one-off via exec (claude print mode is non-interactive-friendly):
curl -s -XPOST $API/sandbox/$ID/exec -H 'content-type: application/json' \
  -d '{"cmd":["bash","-lc","ANTHROPIC_API_KEY=sk-ant-... claude -p \"write hello.py\""]}'
# interactive TUI on the host:
docker exec -it -e ANTHROPIC_API_KEY=sk-ant-... s-$ID bash   # then: claude
```

## Operate
```bash
docker compose logs -f sandboxd   # control-plane logs
docker compose ps                 # stack status
docker compose restart sandboxd   # restart control plane
docker ps --filter label=sandboxed.managed=true   # list running sandboxes
```

## Uninstall
```bash
./uninstall.sh            # stop stack + remove all sandboxes + network (KEEPS workspaces)
./uninstall.sh --images   # also remove built images
./uninstall.sh --data     # also DELETE workspaces + state (confirms first)
./uninstall.sh --all      # images + data
./uninstall.sh --all --yes   # no prompt
```

## Troubleshooting
- **`readyz` not "ready" / "docker info: exit status 1"** — the control plane
  can't reach the Docker socket. Ensure `/var/run/docker.sock` is mounted (it is,
  in compose) and the daemon is running.
- **Port 80 already in use** — set `HTTP_PORT` in `.env` (e.g. `8088`) and
  `docker compose up -d`. Preview URLs then need that port.
- **`id must be a ULID`** — you passed a non-ULID `id`. Omit `id` to auto-generate.
- **Preview shows "Spinning up your app…"** — the sandbox was stopped and is
  waking; it also shows if nothing is listening on the requested port yet.
- **Seeding/permission errors on create** — the daemon likely uses
  `userns-remap`; keep the default `SANDBOXED_USERNS=host`.
