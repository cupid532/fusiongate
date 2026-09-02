#!/usr/bin/env bash
#
# Deploy FusionGate from a commit that is already on origin/main.
#
# The guarantee this script exists to provide: whatever is running on the host
# is byte-identical to a commit anyone can `git fetch` from GitHub. It refuses
# to build anything else. That is why every check below is a hard failure
# rather than a warning — a deploy that "mostly" matches origin is the failure
# mode this is designed to make impossible.
#
# It replaces an undocumented flow that had drifted badly: the build context
# /opt/fusiongate/app was a plain directory with no .git, so nothing tied the
# running image to a commit, and nothing stopped a local edit from being built
# and shipped without ever reaching GitHub.
#
# Usage:
#   deploy/deploy-from-origin.sh              # deploy origin/main
#   deploy/deploy-from-origin.sh <commit>     # deploy a specific pushed commit
#   FUSIONGATE_ALLOW_SAME_VERSION=1 ...       # permit a redeploy of the same version
#   FUSIONGATE_DRY_RUN=1 ...                  # run every check, build nothing
#
set -Eeuo pipefail

FUSIONGATE_HOME="${FUSIONGATE_HOME:-/opt/fusiongate}"
REPO_DIR="${FUSIONGATE_REPO_DIR:-/root/work/fusiongate-ui-strategy}"
COMPOSE_FILE="${FUSIONGATE_COMPOSE_FILE:-$FUSIONGATE_HOME/docker-compose.yml}"
BUILD_ROOT="$FUSIONGATE_HOME/releases"
HEALTH_URL="${FUSIONGATE_HEALTH_URL:-http://127.0.0.1:18787/healthz}"
PUBLIC_HEALTH_URL="${FUSIONGATE_PUBLIC_HEALTH_URL:-https://api.codelee.de/healthz}"
KEEP_ROLLBACK_IMAGES="${FUSIONGATE_KEEP_ROLLBACK_IMAGES:-3}"
KEEP_RELEASES="${FUSIONGATE_KEEP_RELEASES:-3}"
HEALTH_TIMEOUT_SECONDS="${FUSIONGATE_HEALTH_TIMEOUT_SECONDS:-120}"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m!!!\033[0m %s\n' "$*" >&2; exit 1; }

[[ ${EUID:-$(id -u)} -eq 0 ]] || die "must run as root (docker + $FUSIONGATE_HOME)"
command -v docker >/dev/null || die "docker not found"
command -v git    >/dev/null || die "git not found"
command -v jq     >/dev/null || die "jq not found (apt install jq)"
[[ -f "$COMPOSE_FILE" ]] || die "compose file not found: $COMPOSE_FILE"
[[ -d "$REPO_DIR/.git" ]] || die "not a git checkout: $REPO_DIR"

# ---------------------------------------------------------------------------
# 1. Establish what origin actually has
# ---------------------------------------------------------------------------
log "fetching origin"
git -C "$REPO_DIR" fetch origin --prune --quiet

REQUESTED="${1:-origin/main}"
TARGET_SHA="$(git -C "$REPO_DIR" rev-parse --verify "${REQUESTED}^{commit}" 2>/dev/null)" \
  || die "cannot resolve '$REQUESTED' to a commit"
ORIGIN_SHA="$(git -C "$REPO_DIR" rev-parse --verify origin/main)"

# The commit must be an ancestor of (or equal to) origin/main. Anything else is
# either unpushed local work or a side branch, and shipping it would break the
# invariant that the host runs what GitHub serves.
if ! git -C "$REPO_DIR" merge-base --is-ancestor "$TARGET_SHA" "$ORIGIN_SHA"; then
  die "commit $TARGET_SHA is not on origin/main — push it first (git push origin main)"
fi

if [[ "$TARGET_SHA" != "$ORIGIN_SHA" ]]; then
  warn "deploying $(git -C "$REPO_DIR" rev-parse --short "$TARGET_SHA"), which is BEHIND origin/main"
  warn "  origin/main is $(git -C "$REPO_DIR" rev-parse --short "$ORIGIN_SHA")"
  warn "  this is a rollback deploy; the host will not match origin/main afterwards"
fi

# A dirty tree means the thing in front of you is not the thing you are about to
# ship. Refuse, so the difference can never be silently baked into an image.
if [[ -n "$(git -C "$REPO_DIR" status --porcelain)" ]]; then
  git -C "$REPO_DIR" status --short >&2
  die "working tree is dirty — commit and push, or stash, before deploying"
fi

TARGET_SHORT="$(git -C "$REPO_DIR" rev-parse --short "$TARGET_SHA")"
TARGET_VERSION="$(git -C "$REPO_DIR" show "$TARGET_SHA:internal/fusiongate/version.go" \
  | sed -n 's/^const Version = "\(.*\)"$/\1/p')"
[[ -n "$TARGET_VERSION" ]] || die "could not read Version from $TARGET_SHA"
log "target: $TARGET_SHORT  $TARGET_VERSION  $(git -C "$REPO_DIR" log -1 --format=%s "$TARGET_SHA")"

# ---------------------------------------------------------------------------
# 2. Compare against what is running
# ---------------------------------------------------------------------------
RUNNING_JSON="$(curl -fsS --max-time 10 "$HEALTH_URL" 2>/dev/null || echo '{}')"
RUNNING_SHA="$(jq -r '.revision // "unknown"' <<<"$RUNNING_JSON")"
RUNNING_VERSION="$(jq -r '.version // "unknown"' <<<"$RUNNING_JSON")"
log "running: ${RUNNING_SHA:0:7}  $RUNNING_VERSION"

if [[ "$RUNNING_SHA" == "$TARGET_SHA" ]]; then
  log "host already runs this exact commit; nothing to do"
  exit 0
fi

# AGENTS.md requires a version bump with every change. Catching a missing bump
# here rather than after the fact keeps the sidebar version meaningful as a
# deploy marker.
if [[ "$RUNNING_VERSION" == "$TARGET_VERSION" && "${FUSIONGATE_ALLOW_SAME_VERSION:-0}" != "1" ]]; then
  die "version is still $TARGET_VERSION but the commit changed — bump internal/fusiongate/version.go (or set FUSIONGATE_ALLOW_SAME_VERSION=1)"
fi

if [[ "${FUSIONGATE_DRY_RUN:-0}" == "1" ]]; then
  log "dry run: all preflight checks passed, stopping before build"
  exit 0
fi

# ---------------------------------------------------------------------------
# 3. Build from an immutable export of that commit
# ---------------------------------------------------------------------------
# git archive, not a copy of the working tree: the build context can then only
# contain what is committed at that SHA. Uncommitted files cannot leak in even
# by accident.
BUILD_DIR="$BUILD_ROOT/$TARGET_SHORT"
log "exporting $TARGET_SHORT to $BUILD_DIR"
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"
git -C "$REPO_DIR" archive --format=tar "$TARGET_SHA" | tar -x -C "$BUILD_DIR"

CANDIDATE_IMAGE="fusiongate:candidate-$TARGET_SHORT"
log "building $CANDIDATE_IMAGE (the live container keeps serving during this)"
docker build \
  --build-arg "FUSIONGATE_BUILD_REVISION=$TARGET_SHA" \
  --build-arg "FUSIONGATE_BUILD_VERSION=$TARGET_VERSION" \
  --build-arg "FUSIONGATE_BUILD_SOURCE=https://github.com/cupid532/fusiongate" \
  -t "$CANDIDATE_IMAGE" \
  -f "$BUILD_DIR/Dockerfile" "$BUILD_DIR"

# Verify the built artifact reports the identity we asked for, before it is
# allowed anywhere near production traffic.
BUILT_REVISION="$(docker image inspect "$CANDIDATE_IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')"
BUILT_VERSION="$(docker image inspect "$CANDIDATE_IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')"
[[ "$BUILT_REVISION" == "$TARGET_SHA" ]]     || die "image revision label $BUILT_REVISION != $TARGET_SHA"
[[ "$BUILT_VERSION"  == "$TARGET_VERSION" ]] || die "image version label $BUILT_VERSION != $TARGET_VERSION"

# ---------------------------------------------------------------------------
# 4. Swap, health-gate, roll back on failure
# ---------------------------------------------------------------------------
ROLLBACK_IMAGE="fusiongate:rollback-$STAMP"
if docker image inspect fusiongate:local >/dev/null 2>&1; then
  docker tag fusiongate:local "$ROLLBACK_IMAGE"
  echo "$ROLLBACK_IMAGE" > "$FUSIONGATE_HOME/last-rollback-image"
  log "tagged current image as $ROLLBACK_IMAGE"
else
  ROLLBACK_IMAGE=""
  warn "no existing fusiongate:local to tag; rollback will not be available"
fi

# The quality detector runs with network_mode: service:fusiongate, so it shares
# FusionGate's network namespace. Recreating fusiongate alone destroys that
# namespace and leaves the detector attached to nothing — they must always come
# up as a pair.
recreate() {
  docker tag "$1" fusiongate:local
  docker compose -f "$COMPOSE_FILE" up -d --no-build --force-recreate fusiongate quality-detector
}

wait_healthy() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT_SECONDS)) fg qd
  while (( SECONDS < deadline )); do
    fg="$(docker inspect -f '{{.State.Health.Status}}' fusiongate 2>/dev/null || echo missing)"
    qd="$(docker inspect -f '{{.State.Health.Status}}' fusiongate-quality-detector 2>/dev/null || echo missing)"
    [[ "$fg" == "healthy" && "$qd" == "healthy" ]] && return 0
    [[ "$fg" == "unhealthy" ]] && { warn "fusiongate reported unhealthy"; return 1; }
    sleep 3
  done
  warn "timed out after ${HEALTH_TIMEOUT_SECONDS}s (fusiongate=$fg quality-detector=$qd)"
  return 1
}

log "recreating fusiongate + quality-detector"
recreate "$CANDIDATE_IMAGE"

if ! wait_healthy; then
  docker logs fusiongate --tail 40 2>&1 | sed 's/^/    /' >&2
  if [[ -n "$ROLLBACK_IMAGE" ]]; then
    warn "rolling back to $ROLLBACK_IMAGE"
    recreate "$ROLLBACK_IMAGE"
    wait_healthy && warn "rollback is healthy; deploy of $TARGET_SHORT abandoned" \
                 || die "rollback ALSO failed — manual intervention required"
  fi
  die "deploy of $TARGET_SHORT failed health checks"
fi

# ---------------------------------------------------------------------------
# 5. Prove the running process is the commit on origin
# ---------------------------------------------------------------------------
LIVE_JSON="$(curl -fsS --max-time 10 "$HEALTH_URL")" || die "health endpoint unreachable after deploy"
LIVE_SHA="$(jq -r '.revision' <<<"$LIVE_JSON")"
LIVE_VERSION="$(jq -r '.version' <<<"$LIVE_JSON")"
[[ "$LIVE_SHA" == "$TARGET_SHA" ]]        || die "running revision $LIVE_SHA != deployed $TARGET_SHA"
[[ "$LIVE_VERSION" == "$TARGET_VERSION" ]] || die "running version $LIVE_VERSION != $TARGET_VERSION"

if ! curl -fsS --max-time 15 "$PUBLIC_HEALTH_URL" >/dev/null; then
  warn "public endpoint $PUBLIC_HEALTH_URL did not respond; check Caddy"
fi

echo "$TARGET_SHA" > "$FUSIONGATE_HOME/last-deployed-commit"

# ---------------------------------------------------------------------------
# 6. Keep the disk from refilling
# ---------------------------------------------------------------------------
# This host reached 83% full on ~44 accumulated fusiongate tags plus build
# cache. Every commit is on GitHub and rebuildable, so a handful of rollback
# points is all that is worth keeping.
log "pruning old build artifacts"
mapfile -t old_images < <(
  docker images 'fusiongate:rollback-*' --format '{{.CreatedAt}}\t{{.Repository}}:{{.Tag}}' \
    | sort -r | tail -n "+$((KEEP_ROLLBACK_IMAGES + 1))" | cut -f2
)
for image in "${old_images[@]:-}"; do
  [[ -n "$image" ]] && docker rmi "$image" >/dev/null 2>&1 || true
done
docker images 'fusiongate:candidate-*' --format '{{.Repository}}:{{.Tag}}' \
  | grep -v "^${CANDIDATE_IMAGE}$" | xargs -r -n1 docker rmi >/dev/null 2>&1 || true
( cd "$BUILD_ROOT" && ls -1dt */ 2>/dev/null | tail -n "+$((KEEP_RELEASES + 1))" | xargs -r rm -rf )
docker builder prune -f --keep-storage 2GB >/dev/null 2>&1 || true
docker image prune -f >/dev/null 2>&1 || true

cat <<SUMMARY

$(printf '\033[1;32m==>\033[0m') deployed successfully
    commit   $TARGET_SHA
    version  $TARGET_VERSION
    origin   $([[ "$TARGET_SHA" == "$ORIGIN_SHA" ]] && echo "in sync with origin/main" || echo "BEHIND origin/main (rollback deploy)")
    rollback ${ROLLBACK_IMAGE:-none}
    disk     $(df -h / | awk 'NR==2 {print $4" free ("$5" used)"}')
SUMMARY
