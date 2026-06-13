# syntax=docker/dockerfile:1.6
#
# sandbox-base:0.4.0 — durable base image (Phase 2 lineage).
#
# 0.3.0 adds the in-sandbox supervisor `runtimed` as the container's
# main process (CMD) — see control-plane/cmd/runtimed.
#
# Successor to Phase 1's sandbox-base:0.1.0 (validation image). Phase 2
# trims the validation-only tooling (stress-ng, netcat-openbsd), bakes
# in the canonical /home/sandbox skeleton at /opt/sandbox-skel/, and
# pins package-manager cache locations under /home/sandbox/.cache/ so a
# returning user resumes with hot caches across container restart.
#
# The runtime container's entrypoint is intentionally minimal (tini
# only). All home-directory seeding happens once, host-side, via
# `image/scripts/provision-home` (which docker-runs this image with
# `--user 0` to do the copy under Docker's userns-remap). The runtime
# container itself never seeds, never checks emptiness, and never
# branches on first-mount state. Phase 4's Go control plane will own
# this same mechanic; the helper scripts are scaffolding only.
#
# Phase 1 packaging lessons baked in (the roadmap's pseudocode predates
# them; ops/implementation/phase-1-report.md §6 has the full audit):
#   - `unzip` and `passwd` added to the apt list (debian-stable-slim
#     omits them; bun installer and `groupadd` both fail without them).
#   - `/usr/sbin/{groupadd,useradd}` called by absolute path (the
#     image's runtime PATH omits /usr/sbin since the container runs as
#     the unprivileged sandbox user, so admin tools don't belong on
#     PATH at runtime).
#   - `npm install -g pnpm@${PNPM_VERSION}` instead of corepack — Node
#     20's bundled corepack auto-resolves to pnpm 11.x even with
#     `corepack prepare --activate`, and pnpm 11 needs Node 22+ so the
#     runtime crashes on `node:sqlite` import.
#   - `env UV_INSTALL_DIR=/usr/local sh` instead of mv-after-install —
#     the uv 0.4 installer moved its default target from ~/.local/bin
#     to ~/.cargo/bin, and treats `UV_INSTALL_DIR` as a prefix that
#     gets `/bin` appended.
#   - `COPY etc/npmrc` to BOTH `/etc/npmrc` AND `/usr/etc/npmrc` — Node
#     20 from NodeSource sets npm-prefix to `/usr`, so its globalconfig
#     is `/usr/etc/npmrc`; a file at `/etc/npmrc` alone is silently
#     bypassed (`npm config get globalconfig` proves it).
#   - NPM_CONFIG_REGISTRY + PIP_INDEX_URL + PIP_EXTRA_INDEX_URL +
#     PIP_TRUSTED_HOST + BUN_CONFIG_REGISTRY in the image ENV block —
#     env vars beat config files in every package manager and don't
#     depend on file location. The roadmap's `PNPM_REGISTRY` env var
#     is dropped because it doesn't exist for pnpm.

# --- Stage 1: build runtimed (the in-sandbox supervisor) -------------
# Compiling here means the host only needs Docker, not a Go toolchain.
# CGO disabled -> a static binary that runs in the slim base with no
# extra runtime deps. Build context is the repo ROOT (see install.sh
# / docker-compose), so the control-plane source is at ./control-plane.
FROM golang:1.22-bookworm AS runtimed-builder
WORKDIR /src
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
RUN cd control-plane && go mod download
COPY control-plane/ ./control-plane/
RUN cd control-plane && CGO_ENABLED=0 go build -trimpath -o /runtimed ./cmd/runtimed

# --- Stage 2: the sandbox base image ---------------------------------
FROM debian:stable-slim

ARG NODE_VERSION=20.x
ARG PNPM_VERSION=9.12.0
ARG BUN_VERSION=1.1.30
ARG UV_VERSION=0.4.20

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    PATH=/usr/local/bin:/usr/bin:/bin:/home/sandbox/.local/bin:/home/sandbox/.bun/bin

# Strip docs/locales/man pages aggressively from anything we install
# below. dpkg consults this file at install time so the excluded paths
# never land in the image — meaningful trim with no behavior change.
RUN printf 'path-exclude /usr/share/doc/*\npath-exclude /usr/share/man/*\npath-exclude /usr/share/locale/*\n' \
      > /etc/dpkg/dpkg.cfg.d/01-nodoc

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg git ripgrep fd-find jq unzip \
      passwd \
      build-essential python3 python3-venv \
      tini \
 && apt-get clean && rm -rf /var/lib/apt/lists/* /var/cache/apt /var/log/apt

# Node LTS from NodeSource.
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_VERSION%.x}.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && apt-get clean && rm -rf /var/lib/apt/lists/* /var/cache/apt

# pnpm — direct npm install (Phase 1 lesson: corepack auto-latest).
RUN npm install -g pnpm@${PNPM_VERSION} \
 && npm cache clean --force

# uv via the documented UV_INSTALL_DIR prefix.
RUN curl -fsSL https://astral.sh/uv/${UV_VERSION}/install.sh \
     | env UV_INSTALL_DIR=/usr/local sh \
 && uv --version

# bun.
RUN curl -fsSL https://bun.sh/install | bash -s "bun-v${BUN_VERSION}" \
 && mv /root/.bun/bin/bun /usr/local/bin/bun \
 && rm -rf /root/.bun

# Claude Code + OpenCode (npm globals).
#RUN npm install -g @anthropic-ai/claude-code opencode-ai \
# && npm cache clean --force


# Install Claude Code (official native method)
RUN curl -fsSL https://claude.ai/install.sh | bash && \
    cp /root/.local/bin/claude /usr/local/bin/claude && \
    chmod 755 /usr/local/bin/claude

# Registry-proxy config + shell env file. /etc/npmrc and /usr/etc/npmrc
# both ship the same content; /usr/etc/npmrc is the path Node 20's npm
# treats as globalconfig (see header), /etc/npmrc is the documented
# location for portability with other Node versions.
COPY image/etc/npmrc /etc/npmrc
COPY image/etc/npmrc /usr/etc/npmrc
COPY image/etc/pip.conf /etc/pip.conf
COPY image/etc/profile.d/sandbox-env.sh /etc/profile.d/sandbox-env.sh

# Canonical home skeleton — consumed only by host-side provision-home.
# Never read by the runtime container itself. See image/HOME_LAYOUT.md
# for the contract.
COPY image/skel/ /opt/sandbox-skel/

# Sandbox user (uid/gid 1000 inside the container; userns-remap offsets
# it on the host so a running sandbox's processes appear as host-uid
# DOCKREMAP_BASE+1000, typically 101000).
RUN /usr/sbin/groupadd -g 1000 sandbox \
 && /usr/sbin/useradd -u 1000 -g 1000 -m -s /bin/bash sandbox \
 && chown -R 1000:1000 /opt/sandbox-skel \
 && chmod 700 /opt/sandbox-skel

# Public registries. The original platform pointed these at a host-side
# nginx caching proxy (172.17.0.1:8080) for cross-tenant dedupe; the
# portable OSS build talks to the public registries directly so there is
# nothing extra to run. Operators who want the caching proxy can run
# their own and override NPM_CONFIG_REGISTRY / BUN_CONFIG_REGISTRY /
# PIP_INDEX_URL at `docker run` time. Cache dirs still live under
# /home/sandbox/.cache so a returning user resumes with hot caches.
ENV NPM_CONFIG_REGISTRY=https://registry.npmjs.org/ \
    BUN_CONFIG_REGISTRY=https://registry.npmjs.org/ \
    PIP_INDEX_URL=https://pypi.org/simple/

# runtimed — the in-sandbox supervisor, compiled in stage 1. It becomes
# the container's main process below.
COPY --from=runtimed-builder /runtimed /usr/local/bin/runtimed

USER sandbox
WORKDIR /home/sandbox/workspace

# tini reaps zombies (coding agents fork heavily); runtimed is the
# container's main process — it supervises the dev server and runs
# coding tasks. provision-home (host-side) remains the only path that
# seeds /home/sandbox.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/runtimed"]
