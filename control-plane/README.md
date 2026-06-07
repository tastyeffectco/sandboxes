# sandboxd — control plane

The Go control plane for [sandboxd](../README.md). A single binary that drives
the Docker daemon (via the `docker` CLI), stores state in SQLite, runs the idle
and pressure reapers, and serves the HTTP API and the wake path.

See [`../ARCHITECTURE.md`](../ARCHITECTURE.md) for the full design.

## Build & test

```bash
go build ./...
go test ./...
go vet ./...
```

`sandboxd` uses cgo (mattn/go-sqlite3); the container build (`Dockerfile`) sets
`CGO_ENABLED=1`. `runtimed` (`cmd/runtimed`) is CGO-free and is compiled into
the sandbox base image instead.

## Packages

| Package | Responsibility |
|---|---|
| `cmd/sandboxd` | daemon entrypoint, env wiring, background goroutines |
| `cmd/runtimed` | in-sandbox supervisor (baked into the base image) |
| `internal/docker` | thin typed wrapper over the `docker` CLI |
| `internal/loopback` | per-sandbox workspace storage (directory-backed) |
| `internal/traefik` | preview-route label generation |
| `internal/reaper` | idle (stop-on-idle) + host-memory pressure reapers |
| `internal/wake` | wake-on-request handler + warming page |
| `internal/reconcile` | boot-time convergence of Docker → SQLite |
| `internal/store` | SQLite access + numbered migrations |
| `internal/api` | HTTP handlers (`/sandbox*`, `/v1/*`, wake, forward-auth) |
| `internal/auth` | service-token + preview-token auth (optional) |

## Configuration

All runtime configuration is via environment variables, set by the compose file
from `../.env`. The ones the OSS build adds or changes:

| Variable | Default | Purpose |
|---|---|---|
| `PREVIEW_DOMAIN` | `localhost` | domain preview URLs hang off |
| `PREVIEW_ENTRYPOINT` | `web` | Traefik entrypoint on preview routers |
| `PREVIEW_TLS` | `false` | emit `tls=true` on preview routers |
| `SANDBOXD_NETWORK` | `sandboxd_net` | docker network sandboxes join |
| `SANDBOXD_USERNS` | `host` | `--userns` for sandboxes + the seed container |
| `SANDBOXD_DATA_DIR` | `/var/lib/sandboxed` | workspaces + SQLite + logs |
| `SANDBOXD_SET_MEMORY_HIGH` | `false` | write cgroup `memory.high` (needs host cgroup access) |
| `SANDBOXD_IMAGE` | `sandboxd-base:1.0.0` | per-sandbox base image |
| `SANDBOXD_API_AUTH_DISABLED` | `true` | open API for local use |
| `SANDBOXD_API_TOKENS` | — | `name:secret` pairs for service-token auth |
| `SANDBOXD_IDLE_THRESHOLD_SECONDS` | `2100` | idle window before `docker stop` |

## API sketch

```
POST   /sandbox                      create (body: {"ports":[...]}; id optional)
GET    /sandboxes                    list
GET    /sandbox/{id}                 get
POST   /sandbox/{id}/exec            run a command (non-interactive)
DELETE /sandbox/{id}                 destroy container (workspace kept)
POST   /sandbox/{id}/purge           destroy + delete workspace
POST   /v1/sandboxes/{id}/stop       stop (idle); wakes on next preview hit
POST   /v1/sandboxes/{id}/tasks      submit a coding task to runtimed
PUT    /v1/sandboxes/{id}/files      write files into the workspace
GET    /healthz  GET /readyz         liveness / readiness
```
