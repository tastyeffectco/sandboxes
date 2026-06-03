# Phase 2 — interactive-shell ergonomics only. NOT authoritative.
#
# This file is sourced by /etc/profile (login shells) and by
# /home/sandbox/.bashrc. A bare `docker exec s-id <binary>` does NOT
# see anything set here — it sees only the image's Dockerfile ENV
# block. Two layers, distinct scopes:
#
#   Layer 1 — Dockerfile ENV  (authoritative for ALL processes)
#     NPM_CONFIG_REGISTRY, BUN_CONFIG_REGISTRY, PIP_INDEX_URL,
#     PIP_EXTRA_INDEX_URL, PIP_TRUSTED_HOST, PATH, LANG.
#     If a setting MUST work for non-interactive `docker exec`,
#     it belongs in the Dockerfile ENV block, not here.
#
#   Layer 2 — this file  (interactive-shell ergonomics)
#     Cache-path env vars and $PNPM_HOME-on-PATH. The DEFAULTS for
#     each of these are already under $HOME (so persistence is OK
#     even without the export); the explicit names here are for
#     consistency, discoverability, and per-shell overridability.
#
# See image/HOME_LAYOUT.md "Environment-variable boundary" for the
# full table.

export PNPM_HOME="$HOME/.local/share/pnpm"
export PIP_CACHE_DIR="$HOME/.cache/pip"
export UV_CACHE_DIR="$HOME/.cache/uv"
export BUN_INSTALL_CACHE_DIR="$HOME/.cache/bun"
export PATH="$PNPM_HOME:$HOME/.local/bin:$HOME/.bun/bin:$PATH"
