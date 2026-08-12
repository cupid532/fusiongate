#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

PROJECT_NAME="FusionGate"
DEFAULT_REPOSITORY="cupid532/fusiongate"
FUSIONGATE_HOME="${FUSIONGATE_HOME:-/opt/fusiongate}"
REPOSITORY_OVERRIDE="${FUSIONGATE_REPOSITORY:-}"
REF_OVERRIDE="${FUSIONGATE_REF:-}"
REPOSITORY="${REPOSITORY_OVERRIDE:-$DEFAULT_REPOSITORY}"
REF="${REF_OVERRIDE:-main}"
UPDATE_ONLY=false
SOURCE_REVISION=""
SOURCE_VERSION=""
rollback_pending=false
work=""
[[ "${1:-}" == "--update" ]] && UPDATE_ONLY=true

log() { printf '\033[1;36m[%s]\033[0m %s\n' "$PROJECT_NAME" "$*"; }
die() { printf '\033[1;31m[%s]\033[0m %s\n' "$PROJECT_NAME" "$*" >&2; exit 1; }

[[ ${EUID:-$(id -u)} -eq 0 ]] || die "Run as root: curl ... | sudo bash"
[[ "$FUSIONGATE_HOME" =~ ^/[A-Za-z0-9._/-]+$ ]] || die "FUSIONGATE_HOME must be a simple absolute path"
if $UPDATE_ONLY; then
  [[ -f "$FUSIONGATE_HOME/.fusiongate-install" ]] || die "No managed FusionGate installation found at $FUSIONGATE_HOME"
  FUSIONGATE_DOMAIN="$(sed -n 's/^FUSIONGATE_DOMAIN=//p' "$FUSIONGATE_HOME/config/compose.env" | head -1)"
  installed="$(cat "$FUSIONGATE_HOME/.fusiongate-install")"
  if [[ -z "$REPOSITORY_OVERRIDE" ]]; then REPOSITORY="${installed%@*}"; fi
  if [[ -z "$REF_OVERRIDE" ]]; then REF="${installed#*@}"; fi
elif [[ -d "$FUSIONGATE_HOME" && ! -f "$FUSIONGATE_HOME/.fusiongate-install" ]] && find "$FUSIONGATE_HOME" -mindepth 1 -print -quit | grep -q .; then
  die "$FUSIONGATE_HOME already exists and is not a managed FusionGate installation"
fi
[[ "$REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "Invalid FUSIONGATE_REPOSITORY"
[[ "$REF" =~ ^[A-Za-z0-9._/-]+$ ]] || die "Invalid FUSIONGATE_REF"

install_docker() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    return
  fi
  [[ -r /etc/os-release ]] || die "Only Debian and Ubuntu servers are currently supported"
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID:-}" in debian|ubuntu) ;; *) die "Only Debian and Ubuntu servers are currently supported" ;; esac
  log "Installing Docker Engine from Docker's official apt repository"
  apt-get update
  apt-get install -y ca-certificates curl gnupg openssl tar
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  arch="$(dpkg --print-architecture)"
  codename="${VERSION_CODENAME:-}"
  [[ -n "$codename" ]] || die "Cannot determine distribution codename"
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' "$arch" "$ID" "$codename" > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now docker
}

read_domain() {
  if [[ -z "${FUSIONGATE_DOMAIN:-}" ]]; then
    [[ -r /dev/tty ]] || die "Set FUSIONGATE_DOMAIN for non-interactive installation"
    read -r -p "Domain pointing to this server (for example ai.example.com): " FUSIONGATE_DOMAIN </dev/tty
  fi
  FUSIONGATE_DOMAIN="${FUSIONGATE_DOMAIN#http://}"
  FUSIONGATE_DOMAIN="${FUSIONGATE_DOMAIN#https://}"
  FUSIONGATE_DOMAIN="${FUSIONGATE_DOMAIN%/}"
  [[ "$FUSIONGATE_DOMAIN" =~ ^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$ ]] || die "A valid DNS domain is required"
}

read_admin_password() {
  if [[ -n "${FUSIONGATE_ADMIN_PASSWORD:-}" ]]; then
    [[ ${#FUSIONGATE_ADMIN_PASSWORD} -ge 16 ]] || die "FUSIONGATE_ADMIN_PASSWORD must contain at least 16 characters"
    return
  fi
  if [[ -n "${FUSIONGATE_ADMIN_PASSWORD_FILE:-}" ]]; then
    [[ -r "$FUSIONGATE_ADMIN_PASSWORD_FILE" ]] || die "Cannot read FUSIONGATE_ADMIN_PASSWORD_FILE"
    FUSIONGATE_ADMIN_PASSWORD="$(cat -- "$FUSIONGATE_ADMIN_PASSWORD_FILE")"
    [[ ${#FUSIONGATE_ADMIN_PASSWORD} -ge 16 ]] || die "Administrator password file must contain at least 16 characters"
    return
  fi
  [[ -r /dev/tty ]] || die "Set FUSIONGATE_ADMIN_PASSWORD for non-interactive installation"
  local first second
  read -r -s -p "Administrator password (at least 16 characters, blank to generate): " first </dev/tty
  printf '\n' >/dev/tty
  if [[ -z "$first" ]]; then
    FUSIONGATE_ADMIN_PASSWORD="$(openssl rand -base64 30 | tr -d '\n')"
    GENERATED_ADMIN_PASSWORD=true
    return
  fi
  [[ ${#first} -ge 16 ]] || die "Administrator password must contain at least 16 characters"
  read -r -s -p "Repeat administrator password: " second </dev/tty
  printf '\n' >/dev/tty
  [[ "$first" == "$second" ]] || die "Passwords do not match"
  FUSIONGATE_ADMIN_PASSWORD="$first"
}

fetch_source() {
  local destination="$1" api_url effective_url headers=()
  api_url="https://api.github.com/repos/$REPOSITORY/tarball/$REF"
  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    headers=(-H "Authorization: Bearer $GITHUB_TOKEN")
  fi
  effective_url="$(curl -fL --retry 3 --connect-timeout 15 \
    -H "Accept: application/vnd.github+json" \
    "${headers[@]}" "$api_url" -o "$destination/source.tar.gz" -w '%{url_effective}')"
  SOURCE_REVISION="${effective_url%%\?*}"
  SOURCE_REVISION="${SOURCE_REVISION##*/}"
  [[ "$SOURCE_REVISION" =~ ^[0-9a-fA-F]{40}$ ]] || die "Cannot determine the downloaded source revision"
  mkdir -p "$destination/source"
  tar -xzf "$destination/source.tar.gz" --strip-components=1 -C "$destination/source"
  [[ -f "$destination/source/go.mod" && -f "$destination/source/deploy/compose.production.yml" ]] || die "Downloaded archive is not a FusionGate repository"
  SOURCE_VERSION="$(sed -n 's/^const Version = "\(V[0-9][0-9]*\.[0-9][0-9]\)"$/\1/p' "$destination/source/internal/fusiongate/version.go")"
  [[ "$SOURCE_VERSION" =~ ^V[0-9]+\.[0-9]{2}$ ]] || die "Cannot determine the downloaded source version"
}

compose_release() {
  docker compose \
    --project-directory "$FUSIONGATE_HOME/app" \
    --env-file "$FUSIONGATE_HOME/config/compose.env" \
    -f "$FUSIONGATE_HOME/app/deploy/compose.production.yml" "$@"
}

replace_tree() {
  local source="$1" destination="$2"
  if command -v rsync >/dev/null 2>&1; then
    install -d "$destination"
    rsync -a --delete --exclude='.git' "$source/" "$destination/"
  else
    rm -rf -- "$destination"
    cp -a "$source" "$destination"
  fi
}

wait_for_readiness() {
  for _ in $(seq 1 36); do
    if curl -fsS --connect-timeout 5 "https://$FUSIONGATE_DOMAIN/healthz?include=quality-detector" >/dev/null 2>&1 && \
      compose_release exec -T fusiongate /usr/local/bin/fusiongate-healthcheck >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  return 1
}

rollback_update() {
  log "Readiness failed; restoring the previous release"
  compose_release stop quality-detector fusiongate >/dev/null 2>&1 || true
  replace_tree "$previous/app" "$FUSIONGATE_HOME/app" || return 1
  cp -a "$previous/compose.env" "$FUSIONGATE_HOME/config/compose.env" || return 1
  cp -a "$previous/install" "$FUSIONGATE_HOME/.fusiongate-install" || return 1
  docker image tag "$rollback_app_image" fusiongate:production || return 1
  docker image tag "$rollback_detector_image" fusiongate-quality-detector:4.0.1 || return 1
  install -m 0755 "$FUSIONGATE_HOME/app/deploy/fusiongatectl" /usr/local/bin/fusiongatectl || return 1
  compose_release up -d --force-recreate --no-build fusiongate quality-detector caddy || return 1
  wait_for_readiness || return 1
  docker image rm "$rollback_app_image" "$rollback_detector_image" >/dev/null 2>&1 || true
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ "$status" -ne 0 && "$rollback_pending" == true ]]; then
    if rollback_update; then
      log "The previous release was restored after the update error"
    else
      printf '[%s] Automatic rollback also failed; inspect Docker logs immediately\n' "$PROJECT_NAME" >&2
    fi
  fi
  [[ -z "$work" ]] || rm -rf -- "$work"
  exit "$status"
}

install_docker
command -v openssl >/dev/null 2>&1 || { apt-get update && apt-get install -y openssl; }
command -v sqlite3 >/dev/null 2>&1 || { apt-get update && apt-get install -y sqlite3; }
read_domain

if ! $UPDATE_ONLY; then
  read_admin_password
fi

work="$(mktemp -d)"
trap cleanup EXIT
log "Downloading $REPOSITORY@$REF"
fetch_source "$work"
log "Resolved source revision $SOURCE_REVISION ($SOURCE_VERSION)"

if $UPDATE_ONLY; then
  log "Creating a verified pre-update backup"
  /usr/local/bin/fusiongatectl backup >/dev/null

  log "Validating the candidate source before replacing the active release"
  docker build \
    --build-arg "FUSIONGATE_BUILD_REVISION=$SOURCE_REVISION" \
    --build-arg "FUSIONGATE_BUILD_SOURCE=https://github.com/$REPOSITORY" \
    --build-arg "FUSIONGATE_BUILD_VERSION=$SOURCE_VERSION" \
    -t fusiongate:update-candidate "$work/source"
  docker build \
    --build-arg "FUSIONGATE_BUILD_REVISION=$SOURCE_REVISION" \
    --build-arg "FUSIONGATE_BUILD_SOURCE=https://github.com/$REPOSITORY" \
    --build-arg "FUSIONGATE_BUILD_VERSION=$SOURCE_VERSION" \
    -f "$work/source/deploy/quality-detector.Dockerfile" \
    -t fusiongate-quality-detector:update-candidate "$work/source"

  previous="$work/previous"
  rollback_stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  rollback_app_image="fusiongate:rollback-$rollback_stamp"
  rollback_detector_image="fusiongate-quality-detector:rollback-$rollback_stamp"
  mkdir -p "$previous"
  cp -a "$FUSIONGATE_HOME/app" "$previous/app"
  cp -a "$FUSIONGATE_HOME/config/compose.env" "$previous/compose.env"
  cp -a "$FUSIONGATE_HOME/.fusiongate-install" "$previous/install"
  docker image tag fusiongate:production "$rollback_app_image"
  docker image tag fusiongate-quality-detector:4.0.1 "$rollback_detector_image"
fi

install -d -m 0755 "$FUSIONGATE_HOME/app" "$FUSIONGATE_HOME/config"
install -d -m 0700 "$FUSIONGATE_HOME/data" "$FUSIONGATE_HOME/quality-detector-data" "$FUSIONGATE_HOME/caddy-data" "$FUSIONGATE_HOME/caddy-config"
chown 10001:10001 "$FUSIONGATE_HOME/data"
chown 10002:10002 "$FUSIONGATE_HOME/quality-detector-data"

$UPDATE_ONLY && rollback_pending=true
replace_tree "$work/source" "$FUSIONGATE_HOME/app"

if [[ ! -f "$FUSIONGATE_HOME/config/master_key" ]]; then
  master_key="$(openssl rand -base64 32 | tr -d '\n')"
  printf '%s' "$master_key" > "$FUSIONGATE_HOME/config/master_key"
  printf '%s' "$FUSIONGATE_ADMIN_PASSWORD" > "$FUSIONGATE_HOME/config/admin_password"
  chmod 0600 "$FUSIONGATE_HOME/config/master_key" "$FUSIONGATE_HOME/config/admin_password"
fi

if [[ ! -f "$FUSIONGATE_HOME/config/fusiongate.env" ]]; then
  cat > "$FUSIONGATE_HOME/config/fusiongate.env" <<ENV
FUSIONGATE_ALLOW_INSECURE_UPSTREAMS=false
FUSIONGATE_ALLOW_PRIVATE_UPSTREAMS=false
ENV
  chmod 0600 "$FUSIONGATE_HOME/config/fusiongate.env"
fi

cat > "$FUSIONGATE_HOME/config/compose.env" <<ENV
FUSIONGATE_DOMAIN=$FUSIONGATE_DOMAIN
FUSIONGATE_SOURCE_PATH=$FUSIONGATE_HOME/app
FUSIONGATE_BUILD_REVISION=$SOURCE_REVISION
FUSIONGATE_BUILD_SOURCE=https://github.com/$REPOSITORY
FUSIONGATE_BUILD_VERSION=$SOURCE_VERSION
FUSIONGATE_CADDYFILE_PATH=$FUSIONGATE_HOME/app/deploy/Caddyfile
FUSIONGATE_ENV_FILE=$FUSIONGATE_HOME/config/fusiongate.env
FUSIONGATE_MASTER_KEY_PATH=$FUSIONGATE_HOME/config/master_key
FUSIONGATE_ADMIN_PASSWORD_PATH=$FUSIONGATE_HOME/config/admin_password
FUSIONGATE_DATA_PATH=$FUSIONGATE_HOME/data
FUSIONGATE_QUALITY_DETECTOR_DATA_PATH=$FUSIONGATE_HOME/quality-detector-data
CADDY_DATA_PATH=$FUSIONGATE_HOME/caddy-data
CADDY_CONFIG_PATH=$FUSIONGATE_HOME/caddy-config
ENV
chmod 0644 "$FUSIONGATE_HOME/config/compose.env"
printf '%s\n' "$REPOSITORY@$REF" > "$FUSIONGATE_HOME/.fusiongate-install"
install -m 0755 "$FUSIONGATE_HOME/app/deploy/fusiongatectl" /usr/local/bin/fusiongatectl

log "Building and starting production services"
deployment_started=true
compose_release up -d --build --remove-orphans || deployment_started=false
if $deployment_started; then
  compose_release up -d --force-recreate --no-deps quality-detector || deployment_started=false
fi

if [[ "${GENERATED_ADMIN_PASSWORD:-false}" == true ]]; then
  printf '\nGenerated administrator password (shown once):\n%s\n\n' "$FUSIONGATE_ADMIN_PASSWORD"
fi

log "Waiting for the HTTPS endpoint"
healthy=false
if $deployment_started; then
  wait_for_readiness && healthy=true
fi

if $healthy; then
  if $UPDATE_ONLY; then
    rollback_pending=false
    docker image rm "$rollback_app_image" "$rollback_detector_image" >/dev/null 2>&1 || true
  fi
  docker builder prune --force --filter 'until=168h' --keep-storage 5GB >/dev/null 2>&1 || true
  log "FusionGate is online: https://$FUSIONGATE_DOMAIN"
else
  if $UPDATE_ONLY; then
    if rollback_update; then
      rollback_pending=false
      die "Deployment failed readiness checks. The previous release was restored"
    fi
    rollback_pending=false
    die "Deployment and automatic rollback failed. Inspect: docker compose logs"
  fi
  die "Deployment failed readiness checks. Inspect: fusiongatectl logs"
fi
log "Useful commands: fusiongatectl status | logs | health | update | backup"
