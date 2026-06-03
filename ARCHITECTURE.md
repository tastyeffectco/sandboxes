# Architecture

sandboxed is a small Go control plane (`sandboxd`) that drives the Docker
daemon, fronted by Traefik. Everything runs as containers on one host.

```
                         ┌── host (Docker daemon) ─────────────────────────┐
   browser  ──HTTP──▶ :80│  traefik ──┬─▶ s-<id>-3000  (running sandbox)   │
                         │            │      ▲  dev server :3000            │
   API/CLI ──HTTP──▶ :9090  sandboxd ─┼──────┘  (docker run/stop/exec)      │
                         │     │      └─▶ /forward-auth, /wake (catch-all)  │
                         │  SQLite (source of truth)                        │
                         │  reapers: idle (stop) + pressure (mem)           │
                         │  workspaces/  <id>/ … (bind-mounted, persist)    │
                         └──────────────────────────────────────────────────┘
```

## Components

### sandboxd (control plane)
A single Go binary, running in a container with the host Docker socket and the
data directory mounted. It:

- **Owns sandbox lifecycle** — create / list / get / exec / stop / destroy. It
  shells out to the `docker` CLI (`internal/docker`); no SDK.
- **Provisions workspaces** (`internal/loopback`) — one directory per sandbox
  under `SANDBOXED_DATA_DIR/workspaces/<id>`, seeded once from the image's
  `/opt/sandbox-skel`, then bind-mounted into the container at `/home/sandbox`.
- **Emits Traefik labels** (`internal/traefik`) so each sandbox self-registers
  its preview route(s) when it starts.
- **Runs two reapers** (`internal/reaper`): an *idle* reaper that `docker
  stop`s sandboxes idle past a threshold (freeing RAM), and a *pressure* reaper
  that stops sandboxes when host memory runs low.
- **Serves the wake path** (`internal/wake`): the first request to a stopped
  sandbox's preview URL is routed (by a low-priority Traefik catch-all) to
  sandboxd, which `docker start`s the container, waits for the port to come up,
  and serves a styled "warming up" page that auto-refreshes into the app.
- **Reconciles on boot** (`internal/reconcile`): lists Docker containers, diffs
  against SQLite, and converges Docker to the DB. SQLite is always the truth.
- **Stores state** in SQLite (WAL) via `internal/store`; migrations are numbered
  files baked into the image.

### runtimed (in-sandbox supervisor)
Built into the base image as the container's main process (`cmd/runtimed`). It
supervises the user's dev server and runs coding tasks submitted through the
API. It's compiled in the base image's build stage, so the host needs no Go.

### Traefik (edge)
Docker label provider, scoped by a `sandboxed.managed=true` constraint so it
only routes containers this stack owns. Running sandboxes win on a
priority-100 router; the priority-1 file-provider catch-all (`traefik/dynamic/
wake.yml`) forwards anything else to sandboxd's wake path. Plain HTTP by
default; TLS is a config switch (see README → Production / TLS).

## Request flow: first hit to a stopped sandbox

1. Browser → `http://s-<id>-3000.preview.localhost`
2. Container is stopped, so no priority-100 router exists → Traefik's catch-all
   matches and forwards to `sandboxd:9000`.
3. sandboxd checks wake admission (memory headroom), `docker start`s the
   container, polls the port, returns the warming page.
4. The started container's labels make Traefik publish its priority-100 router.
5. The next refresh matches that router and proxies straight to the dev server.

## Isolation model

Each sandbox runs under hardened `runc`: `--cap-drop=ALL`,
`--security-opt=no-new-privileges`, `--read-only` rootfs with `tmpfs` for
`/tmp`, a hard `--memory` ceiling, `--pids-limit`, and file-descriptor ulimits.
The threat model is **authenticated, accountable users running their own code**
— not anonymous hostile multi-tenancy. Kernel-CVE container escape is mitigated
by patching, not by a VM boundary; if you need stronger isolation, run sandboxed
on a dedicated VM per trust domain.

## Storage & persistence

| Class | Where | Survives stop? | Survives reboot? |
|---|---|---|---|
| Workspace | `SANDBOXED_DATA_DIR/workspaces/<id>/` (bind mount) | yes | yes |
| Control-plane state | `SANDBOXED_DATA_DIR/state/sandboxd.db` (SQLite) | yes | yes |
| Container writable layer | none (`--read-only`) | no | no |
| `/tmp`, `/var/tmp` | tmpfs | no | no |

The only writable disk location inside a sandbox is `/home/sandbox`. Back up a
workspace by copying its directory; back up state by copying the SQLite file.

## Deliberate simplifications vs. the original platform

sandboxed is the portable, open-source distribution of a single-node platform
that targeted one specific cloud host. To make it run anywhere with just Docker,
several host-coupled mechanisms were simplified or made optional. Each is a
conscious trade-off:

| Original | sandboxed | Why |
|---|---|---|
| 8 GB ext4 **loopback** per workspace (hard quota) | plain **directory** per workspace | losetup/mkfs/mount need privileged host access; portability beats a hard quota for v1 |
| `memory.high` written to host cgroup | **off by default** (`--memory` ceiling only) | needs host cgroup access the control-plane container may lack |
| **nftables** egress policy + connection logging | none | needs host nftables/journald/systemd |
| host **nginx registry proxy** | public registries | nothing extra to run |
| **DNS-01 wildcard** ACME on a real domain | HTTP on `*.localhost` (TLS optional) | zero-config local; TLS is a documented switch |
| auto-snapshots / Grafana Cloud / systemd timers | dropped | not core to the create→preview→idle→wake loop |

`--userns=host` is set on the infra containers (and, by default, on sandboxes)
so workspace ownership is deterministic whether or not the host daemon uses
userns-remap. Set `SANDBOXED_USERNS=` empty to opt sandboxes back into the
daemon default.
