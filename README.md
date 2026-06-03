<h1 align="center">sandboxed</h1>

<p align="center">
  <b>Self-hosted, isolated Linux dev sandboxes with instant preview URLs — running entirely on Docker.</b>
</p>

<p align="center">
  <i>One command. One host. No Kubernetes.</i>
</p>

---

`sandboxed` gives each user (or each agent, or each branch) an isolated Linux
container with a real shell, the common language toolchains pre-installed, and
an **HTTPS-or-HTTP preview URL** for whatever dev server they run inside it.
Sandboxes **stop when idle** to free RAM and **wake on the next request** —
so you can pack many of them onto one modest host.

It's a small Go control plane that drives the Docker daemon, fronted by
Traefik. That's the whole stack.

> **Driving it from an agent or script?** See [`AGENTS.md`](AGENTS.md) — a
> single self-contained runbook (install → API → agent auth → uninstall).

```
            ┌──────────── your host (just needs Docker) ────────────┐
 browser ──▶│  Traefik  ──▶  sandbox container (dev server :3000)    │
            │     ▲              ▲   ▲   ▲                            │
 API/CLI ──▶│  sandboxd ─────────┘   │   │  (docker run/stop/exec)   │
            │     │  SQLite state     │   └─ workspace dir (persists) │
            └─────┼───────────────────┼────────────────────────────-─┘
                  └─ idle reaper ──────┘  stop-on-idle / wake-on-request
```

## Features

- **Real isolation per sandbox** — hardened `runc`: dropped capabilities,
  `no-new-privileges`, read-only rootfs, per-sandbox memory & PID limits.
- **Preview URLs that just work** — `http://s-<id>-<port>.preview.localhost`
  resolves to your machine with zero DNS and zero certificates. Point it at a
  real wildcard domain for production.
- **Stop-on-idle, wake-on-request** — idle sandboxes are `docker stop`-ed to
  release memory; the first request to a stopped preview wakes it in seconds.
  Workspaces persist across stops and host reboots.
- **Batteries-included image** — Node + pnpm + bun, Python + uv, git, ripgrep,
  fd, plus the Claude Code and OpenCode CLIs, ready to go.
- **A real HTTP API** — create / list / exec / stop / destroy sandboxes,
  write files into a workspace, run coding tasks. Easy to drive from your own
  backend.
- **Single binary control plane** — SQLite is the source of truth; a reconciler
  converges Docker back to it on every boot. No external database, no message
  bus, no orchestrator.

## 1. Install

Requirements: **Docker Engine + the Compose plugin**, on Linux. That's it.

```bash
git clone https://github.com/tastyeffectco/sandboxes.git
cd sandboxes
./install.sh
```

`install.sh` checks Docker, writes a `.env`, builds the sandbox base image + the
control plane, and starts the stack. The API is then live at
`http://127.0.0.1:9090` (verify: `curl http://127.0.0.1:9090/healthz` → `ok`).

## 2. Usage — have OpenCode build an app, then open its preview

The base image already includes the **OpenCode** and **Claude Code** CLIs, so
you can hand a sandbox a prompt and watch it build. Provide an API key at create
time via `env`, then submit a task:

```bash
API=http://127.0.0.1:9090

# (1) create a sandbox on port 3000, with your agent's API key injected
ID=$(curl -s -XPOST $API/sandbox -H 'content-type: application/json' -d '{
        "ports":[3000],
        "env":{"ANTHROPIC_API_KEY":"sk-ant-..."}
     }' | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "sandbox: $ID"

# (2) spin OpenCode with a request — it works in ~/workspace
curl -s -XPOST $API/v1/sandboxes/$ID/tasks -H 'content-type: application/json' -d '{
        "prompt":"create a Vite app that shows a todo list, and run it on port 3000",
        "agent":"opencode"
     }'
# -> {"id":"<taskId>","status":"running","events_url":"/v1/sandboxes/<id>/tasks/<taskId>/events"}

# (3) stream the agent's progress (Server-Sent Events)
curl -N $API/v1/sandboxes/$ID/tasks/<taskId>/events
```

### 3. See the preview URL

Once the app is serving on port 3000, it's live at its preview URL — no extra
wiring, the sandbox self-registered the route:

```
http://s-$ID-3000.preview.localhost
```

`*.localhost` resolves to `127.0.0.1` in every modern browser, so it just works
locally (add `:$HTTP_PORT` if you changed it from 80). The first request to a
stopped sandbox **wakes it** automatically. On a public domain you get
`https://s-<id>-3000.preview.yourdomain.com` (see [Production / TLS](#production--tls)).

> **No agent, just a shell?** Skip step 2 and run anything via the exec API:
> `curl -XPOST $API/sandbox/$ID/exec -d '{"cmd":["bash","-lc","cd ~/workspace && python3 -m http.server 3000"]}'`
> — then open the same preview URL. (exec is non-interactive; for the Claude/
> OpenCode TUI use `docker exec -it s-$ID bash`.)

## API

Base URL = `http://127.0.0.1:9090` (set by `SANDBOXED_API_BIND`). Auth is **off
by default**; with `SANDBOXD_API_AUTH_DISABLED=false` + `SANDBOXD_API_TOKENS`,
add `-H "Authorization: Bearer <secret>"`.

| Method & path | Body | Purpose |
|---|---|---|
| `POST /sandbox` | `{"ports":[3000],"env":{...}}` | **create** — `id` optional (ULID auto), `env` injects vars (e.g. API keys) |
| `GET /sandboxes` | — | list all sandboxes |
| `GET /sandbox/{id}` | — | get one (status, ports, container id…) |
| `POST /sandbox/{id}/exec` | `{"cmd":["bash","-lc","…"]}` | run a command (non-interactive) |
| `POST /sandbox/{id}/keepalive` | — | postpone the idle reaper |
| `POST /v1/sandboxes/{id}/stop` | — | stop now to free RAM (wakes on next preview hit) |
| `DELETE /sandbox/{id}` | — | destroy the container, **keep** the workspace |
| `POST /sandbox/{id}/purge` | — | destroy **and delete** the workspace |
| `POST /v1/sandboxes/{id}/tasks` | `{"prompt":"…","agent":"opencode"}` | run a coding agent headlessly |
| `GET /v1/sandboxes/{id}/tasks/{taskId}` | — | task result |
| `GET /v1/sandboxes/{id}/tasks/{taskId}/events` | — | live task event stream (SSE) |
| `GET/PUT /v1/sandboxes/{id}/files` | `{"path","content","append"}` | list / read / write workspace files |
| `GET /healthz`, `GET /readyz` | — | liveness / readiness |

Full runbook for scripting or driving from an agent: [`AGENTS.md`](AGENTS.md).
Tear down a single sandbox with `DELETE`/`purge`; tear down the whole stack with
`docker compose down` (workspaces persist under `SANDBOXED_DATA_DIR`).

## How it works

| Concern | Choice |
|---|---|
| Container runtime | Docker + hardened `runc` (cap-drop ALL, no-new-privileges, read-only rootfs) |
| Workspace storage | One bind-mounted directory per sandbox under the data dir (persists) |
| Edge / preview | Traefik v3, Docker label provider — sandboxes self-register their routes |
| Idle management | Stop-on-idle (`docker stop`) + wake-on-request; no warm pool |
| State | SQLite (WAL); a reconciler converges Docker to the DB on boot |
| Control plane | One Go binary, shells out to the `docker` CLI over the mounted socket |

The control plane runs in a container with the host Docker socket mounted, and
launches each sandbox as a sibling container on a shared network so Traefik can
route to it. See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the full picture and
the [`control-plane/`](control-plane/) source for the API.

## Configuration

Everything is in `.env` (created from [`.env.example`](.env.example) on first
install). The defaults run a fully working local stack. The knobs you're most
likely to touch:

| Variable | Default | What it does |
|---|---|---|
| `PREVIEW_DOMAIN` | `localhost` | Domain preview URLs hang off |
| `HTTP_PORT` | `80` | Host port Traefik listens on |
| `SANDBOXED_DATA_DIR` | `/var/lib/sandboxed` | Where workspaces + state live |
| `SANDBOXED_API_BIND` | `127.0.0.1:9090` | Where the control-plane API is published |
| `SANDBOXD_API_AUTH_DISABLED` | `true` | Open API for local use; set `false` + tokens for prod |

## Uninstall

```bash
./uninstall.sh            # stop the stack + remove all sandboxes + network (keeps your data)
./uninstall.sh --images   # also remove the built Docker images
./uninstall.sh --data     # also DELETE all workspaces + state (asks to confirm)
./uninstall.sh --all      # full removal: images + data
```

Safe by default — it removes only what sandboxed created (containers labelled
`sandboxed.managed=true`, the compose stack, and the network) and **keeps your
workspaces** under `SANDBOXED_DATA_DIR` unless you pass `--data`/`--all`. After
`--all` you can delete the checkout itself with `rm -rf`.

## Production / TLS

For a public deployment on a real wildcard domain:

1. Point `*.preview.yourdomain.com` at the host.
2. In `traefik/traefik.yml`, enable the `websecure` entrypoint and add a
   certificate resolver (Let's Encrypt DNS-01 is recommended — one wildcard
   cert covers every preview host, so you never hit per-host ACME limits).
3. Set in `.env`: `PREVIEW_DOMAIN=yourdomain.com`, `PREVIEW_ENTRYPOINT=websecure`,
   `PREVIEW_TLS=true`, `HTTP_PORT=80` (kept for the ACME challenge / redirect),
   and **enable auth**: `SANDBOXD_API_AUTH_DISABLED=false` with
   `SANDBOXD_API_TOKENS=name:secret`.
4. `docker compose up -d`.

## Differences from the original platform

`sandboxed` is the open-source distribution of a single-node platform that was
built for a specific cloud host. To make it portable and one-click, a few
host-coupled pieces are simplified or made optional:

- **No hard per-workspace disk quota.** Workspaces are plain directories, not
  loopback ext4 images. The host filesystem is shared.
- **`memory.high` soft throttle is off by default** (needs host cgroup access);
  the hard `--memory` ceiling on each sandbox still applies.
- **No nftables egress policy / connection logging**, and **no host package
  registry proxy** — the image talks to the public npm/PyPI registries.
- **Auto-snapshots are disabled**; the snapshot/template API endpoints exist but
  are experimental on directory storage.

None of these affect the core loop — create, preview, idle, wake, persist. Each
is a deliberate, documented trade-off; see `ARCHITECTURE.md`.

## License

[MIT](LICENSE).
