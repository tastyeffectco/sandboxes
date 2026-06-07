#!/usr/bin/env bash
#
# sandboxd — one-click installer.
#
#   curl -fsSL .../install.sh | bash      # or: ./install.sh
#
# Brings up the whole stack on a single host with nothing but Docker:
#   1. checks Docker + Compose are present
#   2. creates .env from .env.example (if missing)
#   3. builds the sandbox base image and the control-plane image
#   4. creates the data dir
#   5. `docker compose up -d`
#   6. prints how to create your first sandbox
#
# Idempotent: safe to re-run. It never touches anything outside the repo
# and the configured data dir.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

# ── pretty output ────────────────────────────────────────────────────
bold() { printf '\033[1m%s\033[0m\n' "$*"; }
info() { printf '  \033[36m›\033[0m %s\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
die()  { printf '  \033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# ── docker / sudo detection ──────────────────────────────────────────
# Use sudo for docker only if the current user can't reach the daemon.
DOCKER="docker"
if ! docker info >/dev/null 2>&1; then
  if sudo -n docker info >/dev/null 2>&1 || sudo docker info >/dev/null 2>&1; then
    DOCKER="sudo docker"
    info "using 'sudo docker' (current user can't reach the Docker daemon)"
  else
    die "Docker is not available. Install Docker Engine and ensure the daemon is running."
  fi
fi

# Compose v2 (docker compose) preferred; fall back to docker-compose.
if $DOCKER compose version >/dev/null 2>&1; then
  COMPOSE="$DOCKER compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  die "Docker Compose not found. Install the Compose plugin (docker compose)."
fi

bold "sandboxd — installer"
ok "Docker:  $($DOCKER version --format '{{.Server.Version}}' 2>/dev/null || echo present)"
ok "Compose: $($COMPOSE version --short 2>/dev/null || echo present)"

# ── .env ─────────────────────────────────────────────────────────────
if [ ! -f .env ]; then
  cp .env.example .env
  ok "created .env from .env.example"
else
  info ".env already exists — leaving it untouched"
fi

# Load .env so we know the data dir / image tag to use below.
set -a; . ./.env; set +a
DATA_DIR="${SANDBOXD_DATA_DIR:-/var/lib/sandboxed}"
LOG_DIR="${SANDBOXD_LOG_DIR:-$DATA_DIR/log}"
BASE_IMAGE="${SANDBOXD_IMAGE:-sandboxd-base:1.0.0}"

# ── data dir ─────────────────────────────────────────────────────────
# Create it (sudo if we don't own the parent). Workspaces + SQLite + the
# shared access log live here.
if [ ! -d "$DATA_DIR" ]; then
  if mkdir -p "$DATA_DIR" 2>/dev/null; then :; else sudo mkdir -p "$DATA_DIR"; fi
  ok "created data dir $DATA_DIR"
fi
if [ ! -d "$LOG_DIR" ]; then
  if mkdir -p "$LOG_DIR" 2>/dev/null; then :; else sudo mkdir -p "$LOG_DIR"; fi
fi
# Traefik writes the access log here; make sure it can.
( chmod 0777 "$LOG_DIR" 2>/dev/null || sudo chmod 0777 "$LOG_DIR" ) || true
ok "data dir ready: $DATA_DIR"

# ── build the sandbox base image ─────────────────────────────────────
bold "Building the sandbox base image (one-time, a few minutes)…"
DOCKER="$DOCKER" SANDBOXD_IMAGE="$BASE_IMAGE" bash image/build.sh "${BASE_IMAGE##*:}"
ok "base image: $BASE_IMAGE"

# ── build + start the stack ──────────────────────────────────────────
bold "Building the control plane and starting the stack…"
$COMPOSE build
$COMPOSE up -d
ok "stack is up"

# ── summary ──────────────────────────────────────────────────────────
API_BIND="${SANDBOXD_API_BIND:-127.0.0.1:9090}"
HTTP_PORT="${HTTP_PORT:-80}"
PREVIEW_DOMAIN="${PREVIEW_DOMAIN:-localhost}"
PORTSUFFIX=""; [ "$HTTP_PORT" != "80" ] && PORTSUFFIX=":$HTTP_PORT"

echo
bold "sandboxd is running 🎉"
cat <<EOF

  Control-plane API : http://${API_BIND}
  Preview URLs      : http://s-<id>-<port>.preview.${PREVIEW_DOMAIN}${PORTSUFFIX}

  Create your first sandbox (exposing a dev server on port 3000):

    curl -s -XPOST http://${API_BIND}/sandbox \\
      -H 'content-type: application/json' \\
      -d '{"id":"demo01","ports":[3000]}' | tee /dev/stderr

  Then open:  http://s-demo01-3000.preview.${PREVIEW_DOMAIN}${PORTSUFFIX}

  Logs:   $COMPOSE logs -f sandboxd
  Stop:   $COMPOSE down
EOF
