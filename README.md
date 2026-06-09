<h1 align="center">sandboxd</h1>


<p align="center">
  <b>The open-source engine for AI app-builder products.</b><br/>
  Give every user an isolated cloud dev environment, a built-in coding agent,
  and a live preview URL — self-hosted, on one machine, in one command.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-green.svg"></a>
  <img alt="Runs on Docker" src="https://img.shields.io/badge/runs%20on-Docker-2496ED.svg">
  <img alt="Single binary control plane" src="https://img.shields.io/badge/control%20plane-single%20Go%20binary-00ADD8.svg">
  <img alt="Status: beta" src="https://img.shields.io/badge/status-beta-yellow.svg">
</p>

---

<img width="1100" height="816" alt="sandboxd-demo" src="https://github.com/user-attachments/assets/f794ff9b-8ffe-47e8-bd30-22541f870f09" />


## What is sandboxd? (start here)

Think of the apps where you type *"build me a todo app"* and seconds later a
working website appears at its own link — like Lovable, Bolt, v0, or Replit.
**sandboxd is the open-source backend that makes that possible**, running on
your own server.

Here's what it does, in plain terms. You send it one HTTP request, and it:

1. **Creates a sandbox** — a private, isolated Linux container (its own
   filesystem, its own memory limits), so one user's code can never see or
   break another's.
2. **Runs an AI coding agent inside it** — you give it a prompt, and it writes
   the code into that sandbox. (The OpenCode and Claude Code CLIs come
   pre-installed.)
3. **Gives the app a live URL** — the dev server running inside the sandbox is
   instantly reachable at a shareable preview link.

```
POST /sandbox          → a private, isolated container spins up
POST .../tasks         → an AI agent writes an app inside it
http://<id>.preview... → that app is live at its own URL
```

It's also cheap to run: a sandbox **goes to sleep when nobody's using it**
(freeing memory) and **wakes up the instant someone opens its link again** —
files are saved on disk the whole time. So one ordinary server can hold many
users instead of needing one virtual machine each.

Under the hood it's deliberately small and easy to understand: **one Go program
that tells Docker what to do**, with **Traefik** handling the URLs and
**SQLite** as the database. No Kubernetes, no separate database server, no
message queue — you could read the whole thing in an afternoon.

```
            ┌──────────────── your host (just needs Docker) ────────────────┐
 browser ──▶│  Traefik  ──▶  sandbox  (coding agent + dev server :3000)      │
            │     ▲              ▲   ▲                                        │
 API/CLI ──▶│  sandboxd ─────────┘   └─ workspace dir (persists)             │
            │     │  SQLite (source of truth) · idle→stop · request→wake      │
            └─────┴────────────────────────────────────────────────────────-─┘
```

### Who's it for?

**✅ Use it if** you're running **many sandboxes for other people** — an AI
app-builder ("describe an app → see it live"), an agent platform, a coding
playground, per-user or per-branch preview environments, or multi-app hosting
for a team.

**❌ Skip it if** you just need one or two containers for yourself — a shell
script, `docker run`, or [lxd](https://canonical.com/lxd) is simpler. (More on
that [below](#why-not-just-a-shell-script).)

## Why sandboxd?

If you're building an **AI app-builder, an agent platform, a coding playground,
or a per-user preview product**, the hard part isn't the prompt — it's the
infrastructure underneath it:

- **Multi-tenant isolation** so one user's code can't touch another's.
- **Per-user preview URLs** with automatic routing and TLS.
- **Cost control** — idle environments must release memory, or your bill explodes.
- **Agent orchestration** — run a coding agent against a workspace, stream its
  progress, capture the result.
- **Persistence, wake-on-demand, reconciliation after a crash or reboot.**

That's months of platform work. sandboxd is that platform, distilled to one
command:

- ⚡ **One-command install.** `./install.sh` and you have a working API + previews.
- 🧠 **Agents included.** The OpenCode and Claude Code CLIs ship in every sandbox;
  hand a sandbox a prompt and it builds.
- 💸 **Dense by design.** Stop-on-idle + wake-on-request means dozens of sandboxes
  share one box instead of one VM each — the difference between a $20 server and
  a $2,000 cluster.
- 🔓 **Yours.** Self-hosted, MIT-licensed, no vendor lock-in. Own your data, your
  margins, and your roadmap.
- 🪶 **Boring on purpose.** SQLite + the `docker` CLI + Traefik. A reconciler
  converges Docker back to the database on every boot. You can read the whole
  control plane in an afternoon.

## "Why not just a shell script?"

Fair question — and honestly: **if you need one or two long-lived containers for
yourself, a shell script (or `docker run`, or [lxd](https://canonical.com/lxd))
is simpler. Use that.** We mean it. sandboxd is overkill for one-off projects.

It earns its keep the moment you're running **many** sandboxes for **other
people** — a team, or a product — because that's when the tidy little `docker
run` script quietly grows into all of this:

- **URLs, not ports.** Every sandbox gets a clean preview URL with automatic
  routing + TLS — no port bookkeeping, no collisions to manage.
- **It sleeps and wakes itself.** Idle sandboxes stop to free RAM and restart
  transparently on the next request (warming-up page, readiness probe, request
  hold). That part alone is well past 100 lines — and it's the difference
  between one cheap box and a rack of always-on VMs.
- **It survives reboots.** SQLite is the source of truth; a reconciler
  re-converges Docker to it on boot. A script forgets everything when the host
  restarts.
- **It's an API, not a CLI you shell into.** create / exec / stop / destroy /
  write-files / run-agent-task are real HTTP endpoints with auth — you call them
  from your app backend, per user, at scale.
- **One user can't take down the rest.** Per-sandbox memory/PID limits + a
  host-memory pressure reaper.
- **Agents with a lifecycle.** Submit a prompt, stream progress (SSE), capture a
  durable result — not just `opencode` fired inline.

Rebuild those as your script grows and you've rebuilt sandboxd. So: skip it for
one-offs; reach for it when "just a script" has started keeping you up at night.

> **Prefer Kubernetes?** The control plane talks to the container runtime through
> a thin `docker` CLI boundary, so a k8s Job/Pod backend is an interface swap,
> not a rewrite — a great first contribution. Today it targets a single Docker
> host (no k8s required), which is the sweet spot for teams who don't want to run
> a cluster just for sandboxes.

## Quick start

Requirements: **Docker Engine + the Compose plugin**, on Linux. That's it.

### 1. Install

```bash
git clone https://github.com/tastyeffectco/sandboxd.git
cd sandboxd
./install.sh
```

`install.sh` checks Docker, writes a `.env`, builds the sandbox base image + the
control plane, and starts the stack. The API is then live at
`http://127.0.0.1:9090` (verify: `curl http://127.0.0.1:9090/healthz` → `ok`).

### 2. Have an agent build an app

The base image already includes the **OpenCode** and **Claude Code** CLIs. Hand
a sandbox a prompt and watch it build (OpenCode runs on its free plan out of the
box; pass your own provider key via `env` to use your account):

```bash
API=http://127.0.0.1:9090

# create a sandbox that will serve on port 3000
ID=$(curl -s -XPOST $API/sandbox -H 'content-type: application/json' \
       -d '{"ports":[3000]}' | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "sandbox: $ID"

# spin a coding agent with a request — it works in ~/workspace/app
curl -s -XPOST $API/v1/sandboxes/$ID/tasks -H 'content-type: application/json' -d '{
        "prompt":"create a Vite app that shows a todo list and run it on port 3000",
        "agent":"opencode"
     }'
# -> {"id":"<taskId>","status":"running","events_url":"/v1/sandboxes/<id>/tasks/<taskId>/events"}

# stream the agent's progress (Server-Sent Events)
curl -N $API/v1/sandboxes/$ID/tasks/<taskId>/events
```

To use your own model account instead of the free plan, inject a key at create
time — it's available to the agent and any shell in the sandbox:

```bash
curl -s -XPOST $API/sandbox -d '{"ports":[3000],"env":{"ANTHROPIC_API_KEY":"sk-ant-..."}}'
```

### 3. Open the live preview

Once the app serves on port 3000, it's reachable at its preview URL — the
sandbox self-registered the route, nothing else to wire:

```
http://s-<id>-3000.preview.localhost
```

`*.localhost` resolves to `127.0.0.1` in every modern browser, so it works
locally with zero DNS and zero certificates (add `:$HTTP_PORT` if you changed it
from 80). The first request to a stopped sandbox **wakes it** automatically. On a
real domain you get `https://s-<id>-3000.preview.yourdomain.com`
(see [Production / TLS](#production--tls)).

> **Rancher Desktop / Docker Desktop + k3s:** these bundle a k3s cluster whose
> klipper-lb grabs port 80 before sandboxd's Traefik can, so preview URLs return
> 404. Fix: set `HTTP_PORT=8080` in `.env` and run `docker compose up -d traefik`.
> Preview URLs become `http://s-<id>-<port>.preview.localhost:8080`.

> **Just want a shell, no agent?** Skip step 2 and run anything via the exec API:
> `curl -XPOST $API/sandbox/$ID/exec -d '{"cmd":["bash","-lc","cd ~/workspace/app && python3 -m http.server 3000"]}'`
> then open the same preview URL.

## API

Base URL = `http://127.0.0.1:9090` (set by `SANDBOXD_API_BIND`). Auth is **off
by default** for local use; with `SANDBOXD_API_AUTH_DISABLED=false` +
`SANDBOXD_API_TOKENS`, send `-H "Authorization: Bearer <secret>"`.

| Method & path | Body | Purpose |
|---|---|---|
| `POST /sandbox` | `{"ports":[3000],"env":{...}}` | **create** — `id` optional (ULID auto); `env` injects vars (e.g. API keys) |
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

A complete, copy-pasteable runbook (including driving it from your own agent) is
in **[`AGENTS.md`](AGENTS.md)**.

## How it works

| Concern | Choice |
|---|---|
| Container runtime | Docker + hardened `runc` (cap-drop ALL, `no-new-privileges`, read-only rootfs) |
| Workspace storage | one bind-mounted directory per sandbox under the data dir (persists) |
| Edge / preview | Traefik v3 Docker provider — sandboxes self-register their routes |
| Idle management | stop-on-idle (`docker stop`) + wake-on-request; no warm pool |
| State | SQLite (WAL); a reconciler converges Docker to the DB on boot |
| Control plane | one Go binary, shells out to the `docker` CLI over the mounted socket |

The control plane runs in a container with the host Docker socket mounted and
launches each sandbox as a sibling container on a shared network so Traefik can
route to it. Full design: [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Configuration

Everything is in `.env` (created from [`.env.example`](.env.example) on install).
The defaults run a complete local stack. The knobs you'll touch most:

| Variable | Default | What it does |
|---|---|---|
| `PREVIEW_DOMAIN` | `localhost` | domain preview URLs hang off |
| `HTTP_PORT` | `80` | host port Traefik listens on |
| `SANDBOXD_DATA_DIR` | `/var/lib/sandboxed` | where workspaces + state live |
| `SANDBOXD_API_BIND` | `127.0.0.1:9090` | where the control-plane API is published |
| `SANDBOXD_API_AUTH_DISABLED` | `true` | open API for local use; set `false` + tokens for prod |

## Production / TLS

For a public deployment on a real wildcard domain:

1. Point `*.preview.yourdomain.com` at the host.
2. In `traefik/traefik.yml`, enable the `websecure` entrypoint and add a
   certificate resolver (Let's Encrypt DNS-01 is ideal — one wildcard cert covers
   every preview host, so you never hit per-host ACME limits).
3. In `.env`: `PREVIEW_DOMAIN=yourdomain.com`, `PREVIEW_ENTRYPOINT=websecure`,
   `PREVIEW_TLS=true`, and **enable auth** — `SANDBOXD_API_AUTH_DISABLED=false`
   with `SANDBOXD_API_TOKENS=name:secret`.
4. `docker compose up -d`.

## Uninstall

```bash
./uninstall.sh            # stop the stack + remove all sandboxes + network (keeps your data)
./uninstall.sh --images   # also remove the built Docker images
./uninstall.sh --data     # also DELETE all workspaces + state (asks to confirm)
./uninstall.sh --all      # full removal: images + data
```

Safe by default — it removes only what sandboxd created (containers labelled
`sandboxd.managed=true`, the compose stack, the network) and **keeps your
workspaces** unless you pass `--data`/`--all`.

## Is this a good foundation for a startup?

Yes — that's exactly the point. If you want to ship an **AI app-builder or agent
SaaS** without first spending months building multi-tenant isolation, preview
routing, idle/wake cost control, and agent orchestration, sandboxd gives you
that core on day one, on a single inexpensive server, with margins you control.
It's a **strong, honest starting point** — beta-quality, MIT-licensed, and built
to be read and extended. Launch lean on it; harden as you grow (next section).

## Before you scale hard: what's simple on purpose, and what to harden

sandboxd v1 is tuned for "**works anywhere with just Docker, in one command**."
To keep it that simple, a few things were left basic **on purpose**. None of
them affect the core loop (create → build → preview → sleep → wake → persist) —
they're the knobs to tighten once you have real users and real money on the line.
Plain version:

| Kept simple on purpose | Fine for | Do this when you're scaling / serious |
|---|---|---|
| **Container isolation** (hardened Docker), not full VMs | your own users running their own code | running **untrusted strangers' code** → put each tenant on its own VM, or use gVisor / Kata / Firecracker |
| **API auth is OFF by default** | local development | **turn it on** (`SANDBOXD_API_AUTH_DISABLED=false` + tokens) and never expose the API port unauthenticated |
| **Preview links are public** (anyone with the URL) | demos, sharing | gate sensitive previews (the private-sandbox forward-auth hook) |
| **Open, unlogged network egress** | most apps | add firewall / egress rules + logging |
| **Plain-directory workspaces**, no disk quota | a single server | add filesystem/volume quotas; plan multi-host sharding |
| **One server, one Docker socket** (the control plane is root-equivalent on the host) | starting out | treat the host as a trust boundary, keep it patched, isolate it, and don't co-locate unrelated secrets |

**The short version for a fast-scaling company:** the three that matter most are
(1) **stronger isolation** (VM-per-tenant) if you ever run untrusted code,
(2) **turn on API auth** and lock down the host, and (3) **plan for more than one
machine**. Everything else above is a config change, not a rewrite. Start lean,
revisit these as you grow — and PRs are very welcome ([`CONTRIBUTING.md`](CONTRIBUTING.md)).

## License

[MIT](LICENSE). Use it, ship it, sell what you build on it.
