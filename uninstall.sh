#!/usr/bin/env bash
#
# sandboxd — uninstaller.
#
#   ./uninstall.sh                 stop the stack + remove all sandboxes + network
#   ./uninstall.sh --images        also remove the built Docker images
#   ./uninstall.sh --data          also DELETE all workspaces + state (destructive!)
#   ./uninstall.sh --all           --images + --data (full removal)
#   ./uninstall.sh --all --yes     full removal, no confirmation prompt
#
# Safe by default: it removes only what sandboxd created (containers
# carrying the `sandboxd.managed=true` label, plus the compose stack and
# network). Your workspaces under SANDBOXD_DATA_DIR are KEPT unless you
# pass --data / --all. It never touches the git checkout itself.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
info() { printf '  \033[36m›\033[0m %s\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m! %s\033[0m\n' "$*"; }
die()  { printf '  \033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

RM_IMAGES=0; RM_DATA=0; ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    --images) RM_IMAGES=1 ;;
    --data)   RM_DATA=1 ;;
    --all)    RM_IMAGES=1; RM_DATA=1 ;;
    --yes|-y) ASSUME_YES=1 ;;
    *) die "unknown flag: $arg (see header for usage)" ;;
  esac
done

# docker (+ sudo if needed) and compose detection, mirroring install.sh.
DOCKER="docker"
docker info >/dev/null 2>&1 || { sudo docker info >/dev/null 2>&1 && DOCKER="sudo docker" || die "Docker not available."; }
if $DOCKER compose version >/dev/null 2>&1; then COMPOSE="$DOCKER compose"
elif command -v docker-compose >/dev/null 2>&1; then COMPOSE="docker-compose"
else COMPOSE=""; fi

# Load .env (if present) for the data dir / image names.
[ -f .env ] && { set -a; . ./.env; set +a; }
DATA_DIR="${SANDBOXD_DATA_DIR:-/var/lib/sandboxed}"
BASE_IMAGE="${SANDBOXD_IMAGE:-sandboxd-base:1.0.0}"
CP_IMAGE="sandboxd-control-plane:1.0.0"

bold "sandboxd — uninstall"

# 1. Stop + remove the compose stack (traefik + sandboxd) and its network.
if [ -n "$COMPOSE" ]; then
  $COMPOSE down --remove-orphans 2>/dev/null && ok "stopped the stack (traefik + sandboxd)" || info "stack was not running"
fi

# 2. Remove every sandbox container this stack created. Scoped precisely
#    by the sandboxd.managed label so nothing else on the host is touched.
SANDBOXES=$($DOCKER ps -aq --filter "label=sandboxd.managed=true" 2>/dev/null || true)
if [ -n "$SANDBOXES" ]; then
  echo "$SANDBOXES" | xargs -r $DOCKER rm -f >/dev/null
  ok "removed sandbox containers: $(echo "$SANDBOXES" | wc -l | tr -d ' ')"
else
  info "no sandbox containers found"
fi

# 3. Remove the network if it lingers.
NET="${SANDBOXD_NETWORK:-sandboxd_net}"
$DOCKER network rm "$NET" >/dev/null 2>&1 && ok "removed network $NET" || true

# 4. Optionally remove images.
if [ "$RM_IMAGES" = 1 ]; then
  $DOCKER rmi -f "$BASE_IMAGE" "$CP_IMAGE" >/dev/null 2>&1 && ok "removed images" || info "images already gone"
fi

# 5. Optionally delete data (workspaces + SQLite state). Destructive.
if [ "$RM_DATA" = 1 ]; then
  warn "About to DELETE all workspaces + state under: $DATA_DIR"
  if [ "$ASSUME_YES" != 1 ]; then
    read -r -p "  Type 'yes' to confirm: " reply
    [ "$reply" = "yes" ] || die "aborted — data left intact"
  fi
  ( rm -rf "$DATA_DIR" 2>/dev/null || sudo rm -rf "$DATA_DIR" ) && ok "deleted $DATA_DIR"
else
  info "kept your data at $DATA_DIR (use --data to delete it)"
fi

echo
bold "Done."
[ "$RM_IMAGES" = 1 ] && [ "$RM_DATA" = 1 ] \
  && echo "  Fully removed. You can now delete this folder: rm -rf $REPO_ROOT" \
  || echo "  Stack removed. Re-run ./install.sh any time to bring it back."
