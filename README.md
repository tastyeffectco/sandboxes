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

## Quick start

Requirements: **Docker Engine + the Compose plugin**, on Linux. That's it.

```bash
git clone <your-fork-url> sandboxed
cd sandboxed
./install.sh
```

`install.sh` checks Docker, writes a `.env`, builds the sandbox base image and
the control plane, and starts the stack. When it finishes it prints exactly how
to create your first sandbox:

```bash
# create a sandbox that exposes a dev server on port 3000.
# the server generates a ULID id and returns it:
ID=$(curl -s -XPOST http://127.0.0.1:9090/sandbox \
       -H 'content-type: application/json' \
       -d '{"ports":[3000]}' | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "sandbox: $ID"

# start something inside it
curl -s -XPOST http://127.0.0.1:9090/sandbox/$ID/exec \
  -H 'content-type: application/json' \
  -d '{"cmd":["bash","-lc","cd ~/workspace && echo hi > index.html && python3 -m http.server 3000"]}'

# open the preview (browsers resolve *.localhost to 127.0.0.1)
open "http://s-$ID-3000.preview.localhost"
```

> `id` is optional — omit it and a ULID is generated. Pass your own (any
> 26-char [ULID](https://github.com/ulid/spec)) to control it from your backend.

Tear down at any time with `docker compose down`. Your workspaces stay on disk
under `SANDBOXED_DATA_DIR`.

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
