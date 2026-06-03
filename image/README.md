# image/ — Phase 2 base image (+ Phase 4-retired helper scripts)

This directory ships `sandbox-base:0.2.0`, the durable base image
every sandbox container starts from. **The sandbox lifecycle is now
owned by the Phase 4 control plane (`sandboxd`)** — see
`control-plane/README.md`. The helper scripts that used to drive the
lifecycle have been retired to `scripts/dev/` and are kept only as
debugging utilities (see `scripts/dev/README.md`).

The architectural contract for what lives inside a running sandbox is
in **[`HOME_LAYOUT.md`](HOME_LAYOUT.md)** — read that first if you're
new to this repo. Phase 1's runtime-decision deliverable is preserved
at **[`RUNTIME_DECISION.md`](RUNTIME_DECISION.md)** (operator-approved
PROCEED on 2026-05-14).

---

## Layout

```
image/
├── Dockerfile                       # sandbox-base:0.2.0 — durable base image
├── build.sh                         # build helper (linux/arm64, --load into local registry)
├── etc/
│   ├── npmrc                        # registry proxy + pnpm store-dir + prefer-offline
│   ├── pip.conf                     # registry proxy + cache-dir
│   └── profile.d/sandbox-env.sh     # PNPM_HOME + cache env vars; PATH additions
├── skel/                            # canonical /home/sandbox skeleton
│   ├── workspace/.gitkeep
│   ├── .config/.gitkeep
│   ├── .cache/.gitkeep
│   ├── .local/bin/.gitkeep
│   ├── .bun/.gitkeep
│   ├── .bashrc
│   ├── .profile
│   └── .gitconfig
├── scripts/dev/                     # DEBUGGING UTILITIES ONLY (retired by Phase 4)
│   ├── README.md                    #   says "use sandboxd, not these"
│   ├── provision-home               #   idempotent: create+mount+seed loopback
│   ├── release-home                 #   unmount; preserve .img
│   ├── sandbox-up                   #   provision-home + hardened docker run + memory.high (+ optional Traefik labels)
│   ├── sandbox-down                 #   docker rm + release-home
│   ├── sandbox-exec                 #   docker exec shorthand
│   └── set-memory-high              #   /proc/<pid>/cgroup discovery + memory.high write
├── HOME_LAYOUT.md                   # contract — read this first
├── README.md                        # this file
└── RUNTIME_DECISION.md              # Phase 1 deliverable (frozen)
```

> **Supported lifecycle**: `sandboxd` HTTP API on `127.0.0.1:8080`. See
> `control-plane/README.md` for build/install/run + API examples. The
> scripts under `scripts/dev/` do NOT update the SQLite source of
> truth — anything created with them is an orphan as far as `sandboxd`
> is concerned (the reconciler will log it and walk away; auto-adoption
> is intentionally absent in v1, per CLAUDE.md non-negotiable #6).

---

## Phase 0 / Phase 1 substrate dependencies

The host must already be a Phase 0 + Phase 1 frozen host:

- Docker CE with `userns-remap=default` (Phase 0 `daemon.json`).
- `sandbox-registry-proxy` reachable on `172.17.0.1:8080` with the
  `proxy_ignore_headers` + cache-status nginx config landed during
  Phase 1.
- `iptables INPUT -i docker0 -p tcp --dport 8080 -j ACCEPT` persisted
  by `netfilter-persistent`.
- Container DNS pointed at `1.1.1.1, 8.8.8.8` via Phase 0 Patch #6.
- ext4 loopback works (Phase 0 verified), cgroup v2 unified, `nft
  sandbox_platform` table loaded.

If any of that isn't true, `sudo bash host/bootstrap.sh` is the
canonical fix-up. The substrate is frozen at git HEAD — don't change
it from Phase 2 without an explicit operator instruction.

---

## Building the image

```bash
sudo bash image/build.sh                # builds sandbox-base:0.2.0
sudo bash image/build.sh 0.2.1          # iterate on a patch tag
```

`build.sh` runs `docker buildx build --platform linux/arm64 --load`
and reports the uncompressed size. There is no `latest` tag — every
consumer (`provision-home`, `sandbox-up`, the eventual Phase 4
control plane) pins the exact version.

Soft size target: ≤ 500 MB compressed (goal, not a gate). Phase 1's
image was ~1.8 GB uncompressed; the Phase 2 Dockerfile bakes in
aggressive dpkg path-excludes (docs / locales / man pages) plus `npm
cache clean --force` after each global install. If the result lands
above the goal, the trim pass is a future `0.2.1` with a multi-stage
build that drops `build-essential` / `gnupg` from the final layer.

---

## Running a sandbox

Each script is intended to be run with `sudo`:

```bash
sudo bash image/scripts/sandbox-up   <id>            # provision + run
sudo bash image/scripts/sandbox-exec <id> <cmd...>   # exec into it
sudo bash image/scripts/sandbox-down <id>            # stop + release
```

The lifecycle separates **provisioning** (idempotent host-side
loopback create + mount + one-time seed) from **running** (hardened
`docker run` + cgroup `memory.high` write):

- `provision-home <id>` — owns mkfs, mount, and the one-time seed
  from `/opt/sandbox-skel/` inside a one-shot `sandbox-base:0.2.0`
  container. Idempotent: a second call against the same id re-mounts
  if needed and does not re-seed.
- `release-home <id>` — unmounts but preserves the `.img`.

The 8 GiB sparse ext4 loopback lives at
`/var/lib/sandboxed/workspaces/<id>.img` (Phase 1 used a
`phase1/` subdirectory; Phase 2 drops it because workspaces are no
longer phase-scoped). After `sandbox-down`, the `.img` survives; a
subsequent `sandbox-up <id>` re-attaches to it.

---

## The seeding rule

The skel in `/opt/sandbox-skel/` is copied into a fresh loopback
**exactly once**, by `provision-home`. After that, the home belongs
to the user. The runtime container's entrypoint does not seed and
does not check emptiness. If a user wipes a dotfile and wants the
default back, they restore it themselves:

```bash
cp /opt/sandbox-skel/.bashrc ~/.bashrc
```

The skel is readable inside every running container at
`/opt/sandbox-skel/` (owned `sandbox:sandbox`, mode 700). See
[`HOME_LAYOUT.md`](HOME_LAYOUT.md) for the full survival contract.

---

## Rollback

`sandbox-base:0.1.0` from Phase 1 is retained in the local Docker
registry. If `0.2.0` regresses on the host, the rollback steps are:

1. `sudo bash image/scripts/sandbox-down <id>`
2. Edit `IMAGE_TAG=` in `image/scripts/sandbox-up` (and
   `provision-home` if you need to re-seed) back to `sandbox-base:0.1.0`
3. `sudo bash image/scripts/sandbox-up <id>`

**Cross-version `.img` compatibility is argued from the design but
not measured.** A 0.1.0 image against a 0.2.0-seeded loopback will
see extra (Phase 2) skel entries (`.cache/`, `.local/`, `.bun/`,
`.config/`, the dotfiles) — none are *harmful*, but pnpm/pip inside
0.1.0 won't honor Phase 2's `store-dir=` / `cache-dir=` because
those keys aren't in the 0.1.0 image's baked `/etc/npmrc` and
`/etc/pip.conf`. A 0.2.0 image against a 0.1.0-seeded loopback will
have an empty `~/.cache/` and start fresh — no data loss, no cache
hits. **Before relying on rollback on a real workspace, verify on a
throwaway id first.** Validated cross-version migration is deferred
to whichever later phase first needs to migrate a running tenant.

## What carries forward to Phase 4

The Go control plane Phase 4 will own most of what the scripts
currently do. The reusable mechanics are:

- The `provision-home` flow (loopback + mount + seed-via-one-shot-container).
- The hardened `docker run` flag set in `sandbox-up`.
- The `memory.high` discovery + write in `set-memory-high` (via the
  `0::` line in `/proc/<pid>/cgroup`).
- The `dockremap_base + 1000` ownership math (in `provision-home`'s
  pre-seed chown — required because host-uid 0 maps outside the
  userns subuid range).

The scripts themselves are scaffolding and will be removed in Phase
4. Don't grow them into a lifecycle API.
