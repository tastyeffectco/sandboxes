# `/home/sandbox/` — Phase 2 contract

This document is the source of truth for what lives where under
`/home/sandbox` inside a running sandbox container, who owns what, and
what survives each destruction boundary. It is the contract that
Phase 4 (control plane), Phase 7 (snapshots), and later phases all
read from — change the contract here, not in individual phase code.

---

## Layout

```
/home/sandbox/
├── workspace/        # user project code — this is where users cd, clone repos, run dev servers
├── .bashrc           # minimal shell defaults
├── .profile          # sources /etc/profile.d/sandbox-env.sh and ~/.bashrc
├── .gitconfig        # init defaults only; no fake user identity
├── .config/          # tool configs (claude-code, opencode, anything user-installed)
├── .cache/           # package-manager caches:
│   ├── pnpm-store/   #   pnpm content-addressable store
│   ├── pip/          #   pip wheel cache
│   ├── uv/           #   uv cache
│   └── bun/          #   bun install cache
├── .local/           # user-installed binaries + data (pnpm global, uv-installed tools)
│   ├── bin/
│   └── share/pnpm/   #   PNPM_HOME — pnpm's own bin/global packages
└── .bun/             # bun runtime data + bin
```

**Path that downstream phases assume**: user project code lives at
**`/home/sandbox/workspace`**. Do not change this path. Phase 4
control plane assumes it; Phase 7 snapshots assume it; everything
downstream assumes it.

---

## Ownership rule

The loopback's mount root and everything inside it is owned by the
sandbox user (`uid 1000`, `gid 1000`) from inside the container — and
by host-uid `DOCKREMAP_BASE + 1000` (typically `101000`) from the
host's perspective under Docker `userns-remap=default`.

The seeding mechanism (`image/scripts/provision-home`) is what ensures
that. The script chowns the mount root to the dockremap base, then
the seeding container — running under userns-remap — `chown -R`s
everything to `sandbox:sandbox`, which translates to `101000:101000`
on the host. The Phase 2 validation block's check 6 is the proof.

---

## User-modifiable vs platform-managed

In v1, **everything under `/home/sandbox` is user-modifiable**.
There is no protected platform-managed area inside the home. Users can
edit their own `.bashrc`, customise `.gitconfig`, install global pnpm
packages into `~/.local/share/pnpm`, fill `.cache/` to the workspace
quota, and structure `workspace/` however they like.

This may change in later phases (e.g. Phase 8 might add a
`.platform/` directory for token state) — when it does, this section
is the authoritative changelog.

---

## What survives each destruction boundary

| Boundary | Survives? | Notes |
|---|---|---|
| Container restart (`docker restart`) | ✅ Everything in `/home/sandbox` | The container's writable layer is `--read-only` and tmpfs `/tmp` / `/var/tmp` evaporate, but the loopback bind-mount at `/home/sandbox` is untouched. |
| Container destroy + recreate, same id | ✅ Everything | The `.img` file lives on host disk under `/var/lib/sandboxed/workspaces/<id>.img`; `sandbox-down` (and Phase 4's destroy path) preserve it. |
| Container destroy + new id | ❌ Nothing | A new id means a new `.img`. There is no copy-from-id mechanism in v1; if the operator wants that, Phase 7 snapshots are the right primitive. |
| Host reboot | ✅ Everything | The `.img` files are on the ext4 root partition. After reboot, the Phase 4 reconciler re-mounts the relevant loopbacks (until Phase 4 ships, `provision-home <id>` against the existing `.img` is idempotent and re-mounts). |
| Image upgrade WITHIN A MAJOR (e.g. `sandbox-base:0.2.0` → `0.2.1`) | ✅ Everything | The home is a bind mount; rebuilding the image doesn't touch the loopback. Skel changes in `/opt/sandbox-skel/` only affect *new* loopbacks (because seeding happens once at creation). |
| Image change ACROSS A MAJOR (e.g. `0.1.0` ↔ `0.2.0`) | ⚠ "Should work" — **not validated** | The home contract is a superset between versions (0.2.0 added `.cache/`, `.local/`, `.bun/`, `.config/`, dotfiles to the skel; 0.1.0 was bare). Loopbacks are bind-mounted, so file data isn't touched. **But** the image's baked `/etc/npmrc` / `/etc/pip.conf` are version-pinned: a 0.1.0 image against a 0.2.0-seeded loopback won't honor 0.2.0's `store-dir=` / `cache-dir=`, because 0.1.0's files don't have those keys. A 0.2.0 image against a 0.1.0-seeded loopback finds `~/.cache/` empty and starts fresh. Neither direction is *broken*, but cross-major compatibility is argued from the design, not measured. Validate on a throwaway id before doing it on a real workspace. |
| Sandbox archive (Phase 7) | ✅ in tarball | Phase 7's archive flow exports the `.img`. Snapshot/restore semantics are defined there. |

---

## The seeding rule

`/opt/sandbox-skel/` inside the image is the **canonical skeleton**.
It is copied into a new loopback **once, at provision time**, by
`image/scripts/provision-home`. After that, the home belongs to the
user.

Specifically:

- **The runtime container's entrypoint does not seed.** It is `tini`
  + `sleep infinity`. It does not check for emptiness. It does not
  branch on first-mount state. There is one and only one seeding
  path.
- **A second call to `provision-home <existing-id>` does not
  re-seed.** It re-mounts (if needed), but the seed step is gated on
  the mount being empty (other than ext4's `lost+found`). If the user
  has any files in their home, the skel is skipped.
- **If a user wipes a dotfile and wants the default back**, they
  must restore it themselves:
  ```bash
  cp /opt/sandbox-skel/.bashrc ~/.bashrc
  ```
  The `/opt/sandbox-skel/` directory exists inside every running
  container, owned `sandbox:sandbox` with mode 700, so the user can
  always read it. This is documented in `image/README.md` too.

Why one-shot seeding and no entrypoint re-seed: silent re-seeding
would overwrite user customisation, which is the worst failure mode.
The user owns their home; the platform owns the *initial* contents
and nothing else.

---

## Environment-variable boundary: image `ENV` vs `/etc/profile.d/sandbox-env.sh`

The image carries two different env-variable layers and they have
distinct scopes. Confusing them produces silent failures (e.g. a non-
interactive `docker exec ... pnpm install` skipping the registry
proxy).

### Authoritative for ALL processes — Dockerfile `ENV` block

Set in the image at build time. Every process the container ever
spawns — interactive shells, non-interactive `docker exec` commands,
the entrypoint chain (`tini` → `sleep infinity`), background daemons,
anything else — inherits these without needing to source any file.

| Var | Value | Why authoritative |
|---|---|---|
| `NPM_CONFIG_REGISTRY` | `http://172.17.0.1:8080/npm/` | npm + pnpm honor this env var; beats any `.npmrc` file |
| `BUN_CONFIG_REGISTRY` | `http://172.17.0.1:8080/npm/` | bun's documented registry knob |
| `PIP_INDEX_URL` | `http://172.17.0.1:8080/pypi/simple/` | pip + uv (uv reads pip's env block) |
| `PIP_EXTRA_INDEX_URL` | `https://pypi.org/simple/` | wheel-fetch fallback (proxy is simple-index only) |
| `PIP_TRUSTED_HOST` | `172.17.0.1` | required because the proxy speaks plain HTTP |
| `PATH` | `/usr/local/bin:/usr/bin:/bin:/home/sandbox/.local/bin:/home/sandbox/.bun/bin` | every binary the runtime container needs is on PATH without a shell-init step |
| `LANG`, `DEBIAN_FRONTEND` | C.UTF-8 / noninteractive | base locale + apt-build hygiene |

**Rule of thumb**: if a setting MUST work for `docker exec s-id pnpm
install` (no `bash -l`), it belongs in the Dockerfile `ENV` block.

### Interactive-shell ergonomics — `/etc/profile.d/sandbox-env.sh`

Sourced only when `/etc/profile` runs, which means: login shells
(`bash -l`, ssh-style sessions, `docker exec -it … bash -l`), plus
anything that ultimately sources `~/.profile` or `~/.bashrc` (the
skel sources sandbox-env.sh from both). A bare `docker exec s-id
<binary>` does **not** see these.

| Var | Value | Why scoped to interactive shells |
|---|---|---|
| `PNPM_HOME` | `$HOME/.local/share/pnpm` | pnpm's global-install location; only relevant when a user invokes `pnpm i -g …` in a shell |
| `PIP_CACHE_DIR` | `$HOME/.cache/pip` | redundant with `pip.conf`'s `cache-dir=`; here for env-var-only paths |
| `UV_CACHE_DIR` | `$HOME/.cache/uv` | uv has no config-file equivalent; defaults to `$HOME/.cache/uv` anyway, this just makes it explicit |
| `BUN_INSTALL_CACHE_DIR` | `$HOME/.cache/bun` | bun has no config-file equivalent; defaults under `$HOME/.bun/…` otherwise (still persistent, this just pins the documented location) |
| `PATH` prepends | `$PNPM_HOME:$HOME/.local/bin:$HOME/.bun/bin:$PATH` | mostly redundant with the image `ENV` `PATH` (which already contains `$HOME/.local/bin` and `$HOME/.bun/bin`) — adds `$PNPM_HOME` and reorders for de-dup |

**Rule of thumb**: if a setting only matters in an interactive
session (where the user can also run `export FOO=bar` themselves
without losing anything), it belongs in `sandbox-env.sh`. Cache
locations are in this layer because the **defaults are already
under `$HOME`** (persistent in the loopback) — the env vars are
documentation and consistency, not correctness.

### Why this split and not "everything in `ENV`"

- Cache-path env vars in `ENV` would harden them into the image,
  meaning a user who legitimately wants a different cache layout
  (e.g. an offline air-gapped flow) couldn't override per-shell. The
  profile.d approach is `export FOO=…`-style — overridable.
- `ENV` is fixed at image build time. Phase 4's control plane may
  one day want to vary cache paths per-tenant; that's much easier
  if the cache layer lives in a sourced file (control plane writes a
  drop-in alongside `sandbox-env.sh`) than if it lives baked into
  the image.

## Cache locations (and why they live in the loopback)

The image pins package-manager cache paths to subdirectories of
`/home/sandbox/.cache/`:

| Tool | Path | Set by |
|---|---|---|
| pnpm content store | `/home/sandbox/.cache/pnpm-store` | `store-dir=` in `/etc/npmrc` and `/usr/etc/npmrc` |
| pip wheel cache | `/home/sandbox/.cache/pip` | `cache-dir=` in `/etc/pip.conf`, plus `PIP_CACHE_DIR` in `/etc/profile.d/sandbox-env.sh` |
| uv cache | `/home/sandbox/.cache/uv` | `UV_CACHE_DIR` in `/etc/profile.d/sandbox-env.sh` |
| bun install cache | `/home/sandbox/.cache/bun` | `BUN_INSTALL_CACHE_DIR` in `/etc/profile.d/sandbox-env.sh` |

These all live inside the loopback, so a returning user resumes with
hot caches across container restart. Phase 1's image left these in
tmpfs and they were lost on every restart — Phase 2's single biggest
UX win is moving them into the loopback.

The 8 GiB workspace cap accounts for cache growth. If a user fills
the cache, they can `rm -rf ~/.cache/*` themselves; Phase 8 will
surface a per-sandbox "clear caches" command.

---

## Soft size budget

The image is targeted at ≤ 500 MB compressed. This is a goal, not a
gate (Phase 1's image was ~1.8 GB uncompressed; Phase 2 is the trim
pass). If 0.2.0 lands above the goal, the next-iteration `0.2.1`
gets a multi-stage build that drops `build-essential` / `gnupg` from
the final layer. We do **not** drop `--read-only`, change the user
contract, or weaken the locked stack to hit a size number.

---

## How Phase 4 reads this contract

When Phase 4's control plane lands, it will:

- Call the equivalent of `provision-home` from Go for the create path.
- Trust `/home/sandbox/workspace/` as the user-code path for snapshot
  + restore logic (Phase 7).
- Trust `/home/sandbox/.cache/` as a *cacheable* (i.e. losable for
  storage relief) subtree should a future phase need it.
- Never seed automatically — operators or users who want a reset
  copy from `/opt/sandbox-skel/` themselves.

Changes to this contract require a CLAUDE.md amendment.
