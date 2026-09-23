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
BACKUP_DIR="${FUSIONGATE_BACKUP_DIR:-/var/backups/fusiongate}"
BACKUP_KEEP="${FUSIONGATE_BACKUP_KEEP:-14}"
COMPOSE_HINT="docker compose --project-directory $FUSIONGATE_HOME/app --env-file $FUSIONGATE_HOME/config/compose.env -f $FUSIONGATE_HOME/app/deploy/compose.production.yml"
UPDATE_ONLY=false
BACKUP_ONLY=false
RESTORE_FILE=""
SOURCE_REVISION=""
SOURCE_VERSION=""
rollback_pending=false
work=""
exit_handler=""
backup_partial=""
backup_archive=""
backup_complete=false
backup_lock=""
backup_service_stopped=false
restore_work=""
restore_safety=""
restore_active=false
restore_preserve_artifacts=false

log() { printf '\033[1;36m[%s]\033[0m %s\n' "$PROJECT_NAME" "$*"; }
die() { printf '\033[1;31m[%s]\033[0m %s\n' "$PROJECT_NAME" "$*" >&2; exit 1; }
usage() { die "Usage: install.sh [--update | --backup | --restore FILE]"; }

case "${1:-}" in
  "") [[ "$#" -eq 0 ]] || usage ;;
  --update) [[ "$#" -eq 1 ]] || usage; UPDATE_ONLY=true ;;
  --backup) [[ "$#" -eq 1 ]] || usage; BACKUP_ONLY=true ;;
  --restore)
    [[ "$#" -eq 2 && -n "${2:-}" ]] || usage
    RESTORE_FILE="$2"
    ;;
  *) usage ;;
esac

# The published one-line command is also the safe cross-version entry point:
# on a managed host it behaves exactly like --update.
if [[ "$#" -eq 0 && -f "$FUSIONGATE_HOME/.fusiongate-install" ]]; then
  UPDATE_ONLY=true
fi

require_install() {
  [[ -f "$FUSIONGATE_HOME/.fusiongate-install" && -f "$FUSIONGATE_HOME/app/deploy/compose.production.yml" ]] || \
    die "No managed FusionGate installation found at $FUSIONGATE_HOME"
}

[[ ${EUID:-$(id -u)} -eq 0 ]] || die "Run as root: curl ... | sudo bash"
[[ "$FUSIONGATE_HOME" =~ ^/[A-Za-z0-9._/-]+$ ]] || die "FUSIONGATE_HOME must be a simple absolute path"
if $UPDATE_ONLY || $BACKUP_ONLY || [[ -n "$RESTORE_FILE" ]]; then
  require_install
  if [[ -z "${FUSIONGATE_DOMAIN:-}" ]]; then
    FUSIONGATE_DOMAIN="$(sed -n 's/^FUSIONGATE_DOMAIN=//p' "$FUSIONGATE_HOME/config/compose.env" | head -1)"
  fi
  if $UPDATE_ONLY; then
    installed="$(cat "$FUSIONGATE_HOME/.fusiongate-install")"
    if [[ -z "$REPOSITORY_OVERRIDE" ]]; then REPOSITORY="${installed%@*}"; fi
    if [[ -z "$REF_OVERRIDE" ]]; then REF="${installed#*@}"; fi
  fi
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
    if curl -fsS --connect-timeout 5 "https://$FUSIONGATE_DOMAIN/healthz" >/dev/null 2>&1 && \
      compose_release exec -T fusiongate /usr/local/bin/fusiongate-healthcheck >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  return 1
}

wait_for_container_readiness() {
  for _ in $(seq 1 30); do
    if compose_release exec -T fusiongate /usr/local/bin/fusiongate-healthcheck >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

report_public_endpoint() {
  [[ -n "${FUSIONGATE_DOMAIN:-}" ]] || return 0
  if curl -fsS --connect-timeout 5 "https://$FUSIONGATE_DOMAIN/healthz" >/dev/null 2>&1; then
    log "Public health check answered"
  else
    printf '[%s] warning: public health check did not answer; restore succeeded\n' "$PROJECT_NAME" >&2
  fi
}

verify_database() {
  local database="$1" result
  result="$(sqlite3 "$database" 'PRAGMA quick_check; PRAGMA foreign_key_check;')"
  [[ "$result" == "ok" ]] || die "Database integrity check failed: $result"
}

sha256_digest() {
  sha256sum -- "$1" | while read -r digest _; do
    printf '%s\n' "$digest"
  done
}

prune_backups() {
  local archive archives=() keep remove_count i
  [[ "$BACKUP_KEEP" =~ ^[0-9]+$ ]] || die "FUSIONGATE_BACKUP_KEEP must be a positive integer"
  keep=$((10#$BACKUP_KEEP))
  [[ "$keep" -ge 1 ]] || die "FUSIONGATE_BACKUP_KEEP must be a positive integer"
  for archive in "$BACKUP_DIR"/fusiongate-????????T??????Z.tar.gz; do
    [[ -f "$archive" ]] || continue
    archives+=("$archive")
  done
  remove_count=$((${#archives[@]} - keep))
  for ((i = 0; i < remove_count; i++)); do
    rm -f -- "${archives[$i]}" "${archives[$i]}.sha256"
  done
}

rollback_update() {
  log "Readiness failed; restoring the previous release"
  compose_release stop fusiongate >/dev/null 2>&1 || true
  replace_tree "$previous/app" "$FUSIONGATE_HOME/app" || return 1
  cp -a "$previous/compose.env" "$FUSIONGATE_HOME/config/compose.env" || return 1
  cp -a "$previous/install" "$FUSIONGATE_HOME/.fusiongate-install" || return 1
  docker image tag "$rollback_app_image" fusiongate:production || return 1
  compose_release up -d --force-recreate --no-build fusiongate caddy || return 1
  wait_for_readiness || return 1
  docker image rm "$rollback_app_image" >/dev/null 2>&1 || true
}

cleanup() {
  # shellcheck disable=SC2319
  local status=$?
  trap - EXIT
  if [[ "$#" -gt 0 ]]; then status="$1"; fi
  if [[ "$status" -ne 0 && "$rollback_pending" == true ]]; then
    if rollback_update; then
      log "The previous release was restored after the update error"
    else
      printf '[%s] Automatic rollback also failed; inspect Docker logs immediately\n' "$PROJECT_NAME" >&2
    fi
  fi
  [[ -z "$work" ]] || rm -rf -- "$work"
  if [[ "$restore_active" == true ]]; then
    if rollback_swap; then
      restore_active=false
      restore_preserve_artifacts=true
    else
      restore_preserve_artifacts=true
      compose_release stop fusiongate >/dev/null 2>&1 || true
      printf '[%s] Restore rollback was incomplete; service remains stopped. Preserve recovery data at %s\n' \
        "$PROJECT_NAME" "$restore_safety" >&2
    fi
  fi
  if [[ "$restore_preserve_artifacts" != true ]]; then
    [[ -z "$restore_work" ]] || rm -rf -- "$restore_work"
  fi
  exit "$status"
}

backup_exit_handler() {
  local status=$?
  trap - EXIT
  if [[ "$backup_complete" != true ]]; then
    [[ -z "$backup_partial" ]] || rm -f -- "$backup_partial" "$backup_partial.sha256"
    [[ -z "$backup_archive" ]] || rm -f -- "$backup_archive" "$backup_archive.sha256"
  fi
  if [[ -n "$backup_lock" ]]; then rmdir -- "$backup_lock" || true; fi
  if [[ "$backup_service_stopped" == true ]]; then
    compose_release start fusiongate >/dev/null 2>&1 || \
      printf '[%s] warning: fusiongate could not be restarted; start it manually\n' "$PROJECT_NAME" >&2
    backup_service_stopped=false
  fi
  if [[ -n "$exit_handler" ]]; then cleanup "$status"; fi
  exit "$status"
}

run_backup() {
  local stamp archive previous_trap
  previous_trap="$(trap -p EXIT || true)"
  [[ -n "$previous_trap" ]] && exit_handler=cleanup || exit_handler=""
  [[ -f "$FUSIONGATE_HOME/config/master_key" ]] || die "Cannot back up without config/master_key"
  install -d -m 0700 "$BACKUP_DIR"
  backup_lock="$BACKUP_DIR/.backup.lock"
  mkdir -- "$backup_lock" || die "Another backup is running or a stale lock exists: $backup_lock"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  archive="$BACKUP_DIR/fusiongate-$stamp.tar.gz"
  if [[ -e "$archive" || -e "$archive.sha256" ]]; then
    rmdir -- "$backup_lock"
    backup_lock=""
    die "Backup for this second already exists: $archive"
  fi
  backup_archive="$archive"
  backup_partial="$archive.partial"
  backup_complete=false
  backup_service_stopped=true
  trap backup_exit_handler EXIT
  log "Stopping FusionGate and writing $archive"
  compose_release stop fusiongate
  verify_database "$FUSIONGATE_HOME/data/fusiongate.db"
  rm -f -- "$backup_partial" "$backup_partial.sha256"
  tar -C "$FUSIONGATE_HOME" -czf "$backup_partial" data config .fusiongate-install
  chmod 0600 "$backup_partial"
  sha256_digest "$backup_partial" > "$backup_partial.sha256"
  chmod 0600 "$backup_partial.sha256"
  mv "$backup_partial" "$archive"
  mv "$backup_partial.sha256" "$archive.sha256"
  compose_release start fusiongate
  backup_service_stopped=false
  rmdir -- "$backup_lock"
  backup_lock=""
  trap - EXIT
  # shellcheck disable=SC2064
  if [[ -n "$exit_handler" ]]; then trap "$exit_handler" EXIT; fi
  exit_handler=""
  prune_backups
  printf '%s\n' "$archive"
}

swap_data_set() {
  if mv -- "$FUSIONGATE_HOME/data" "$restore_safety/data"; then
    :
  else
    [[ ! -e "$FUSIONGATE_HOME/data" && -d "$restore_safety/data" ]] && return 1
    return 1
  fi
  mv -- "$FUSIONGATE_HOME/config" "$restore_safety/config" || return 1
  mv -- "$restore_work/extracted/data" "$FUSIONGATE_HOME/data" || return 1
  mv -- "$restore_work/extracted/config" "$FUSIONGATE_HOME/config" || return 1
  chown -R 10001:10001 "$FUSIONGATE_HOME/data" || return 1
  chmod 0700 "$FUSIONGATE_HOME/data" "$FUSIONGATE_HOME/config" || return 1
}

rollback_swap() {
  local failed=false
  compose_release stop fusiongate >/dev/null 2>&1 || true
  if [[ -d "$restore_safety/data" ]]; then
    if [[ -e "$FUSIONGATE_HOME/data" ]]; then
      mv -- "$FUSIONGATE_HOME/data" "$restore_safety/failed-data" || failed=true
    fi
    if [[ "$failed" == false ]]; then
      mv -- "$restore_safety/data" "$FUSIONGATE_HOME/data" || failed=true
    fi
  elif [[ ! -d "$FUSIONGATE_HOME/data" ]]; then
    failed=true
  fi
  if [[ -d "$restore_safety/config" ]]; then
    if [[ -e "$FUSIONGATE_HOME/config" ]]; then
      mv -- "$FUSIONGATE_HOME/config" "$restore_safety/failed-config" || failed=true
    fi
    if [[ "$failed" == false ]]; then
      mv -- "$restore_safety/config" "$FUSIONGATE_HOME/config" || failed=true
    fi
  elif [[ ! -d "$FUSIONGATE_HOME/config" ]]; then
    failed=true
  fi
  if [[ "$failed" == true || ! -d "$FUSIONGATE_HOME/data" || ! -d "$FUSIONGATE_HOME/config" ]]; then
    restore_preserve_artifacts=true
    return 1
  fi
  if ! compose_release start fusiongate; then
    compose_release stop fusiongate >/dev/null 2>&1 || true
    restore_preserve_artifacts=true
    return 1
  fi
  return 0
}

run_restore() {
  local archive="$1" expected_checksum actual_checksum listing
  [[ -f "$archive" ]] || die "Backup archive not found: $archive"
  [[ -f "$archive.sha256" ]] || die "Missing checksum: $archive.sha256"
  read -r expected_checksum _ < "$archive.sha256" || true
  [[ "$expected_checksum" =~ ^[[:xdigit:]]{64}$ ]] || die "Invalid checksum in $archive.sha256"
  actual_checksum="$(sha256_digest "$archive")"
  [[ "${actual_checksum,,}" == "${expected_checksum,,}" ]] || die "Checksum mismatch: $archive"

  restore_work="$(mktemp -d "$FUSIONGATE_HOME/.restore-work.XXXXXX")"
  trap cleanup EXIT
  listing="$restore_work/archive.list"
  tar -tzf "$archive" > "$listing" || die "Cannot list backup archive: $archive"
  grep -qx 'data/fusiongate.db' "$listing" || die "Backup does not contain data/fusiongate.db"
  grep -qx 'config/master_key' "$listing" || die "Backup does not contain config/master_key"
  mkdir -p "$restore_work/extracted"
  tar -xzf "$archive" -C "$restore_work/extracted"
  verify_database "$restore_work/extracted/data/fusiongate.db"
  [[ -f "$restore_work/extracted/config/master_key" ]] || die "Backup does not contain config/master_key"

  restore_safety="$(mktemp -d "$FUSIONGATE_HOME/pre-restore-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")"
  log "Stopping FusionGate and preserving the current data set"
  compose_release stop fusiongate
  restore_active=true
  if ! swap_data_set; then
    if rollback_swap; then
      restore_active=false
      restore_preserve_artifacts=true
      trap - EXIT
      die "Restore exchange failed; the previous installation was restored. Failed restore is at $restore_safety"
    fi
    restore_preserve_artifacts=true
    trap - EXIT
    die "Restore exchange failed and rollback was incomplete; service remains stopped. Recovery data: $restore_safety"
  fi

  log "Starting FusionGate with the restored data"
  compose_release start fusiongate
  if wait_for_container_readiness; then
    restore_active=false
    restore_preserve_artifacts=true
    report_public_endpoint
    log "Restored $archive; the previous data set is preserved at $restore_safety"
    trap - EXIT
    rm -rf -- "$restore_work"
    restore_work=""
    return 0
  fi
  if rollback_swap; then
    restore_active=false
    restore_preserve_artifacts=true
    trap - EXIT
    die "Restore did not become healthy; the previous installation was restored. Failed restore is at $restore_safety"
  fi
  restore_preserve_artifacts=true
  trap - EXIT
  die "Restore failed and rollback was incomplete; service remains stopped. Recovery data: $restore_safety"
}

remove_legacy_command() {
  local legacy_cli=/usr/local/bin/fusiongate""ctl
  if [[ -e "$legacy_cli" ]]; then
    if rm -f -- "$legacy_cli"; then
      log "Removing the legacy management command"
    else
      printf '[%s] warning: could not remove the legacy management command\n' "$PROJECT_NAME" >&2
    fi
  fi
}

install_docker
command -v openssl >/dev/null 2>&1 || { apt-get update && apt-get install -y openssl; }
command -v sqlite3 >/dev/null 2>&1 || { apt-get update && apt-get install -y sqlite3; }

if $BACKUP_ONLY; then
  run_backup
  exit 0
fi

if [[ -n "$RESTORE_FILE" ]]; then
  run_restore "$RESTORE_FILE"
  exit 0
fi

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
  run_backup >/dev/null

  log "Validating the candidate source before replacing the active release"
  docker build \
    --build-arg "FUSIONGATE_BUILD_REVISION=$SOURCE_REVISION" \
    --build-arg "FUSIONGATE_BUILD_SOURCE=https://github.com/$REPOSITORY" \
    --build-arg "FUSIONGATE_BUILD_VERSION=$SOURCE_VERSION" \
    -t fusiongate:update-candidate "$work/source"

  previous="$work/previous"
  rollback_stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  rollback_app_image="fusiongate:rollback-$rollback_stamp"
  mkdir -p "$previous"
  cp -a "$FUSIONGATE_HOME/app" "$previous/app"
  cp -a "$FUSIONGATE_HOME/config/compose.env" "$previous/compose.env"
  cp -a "$FUSIONGATE_HOME/.fusiongate-install" "$previous/install"
  docker image tag fusiongate:production "$rollback_app_image"
fi

install -d -m 0755 "$FUSIONGATE_HOME/app" "$FUSIONGATE_HOME/config"
install -d -m 0700 "$FUSIONGATE_HOME/data" "$FUSIONGATE_HOME/caddy-data" "$FUSIONGATE_HOME/caddy-config"
chown 10001:10001 "$FUSIONGATE_HOME/data"

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
CADDY_DATA_PATH=$FUSIONGATE_HOME/caddy-data
CADDY_CONFIG_PATH=$FUSIONGATE_HOME/caddy-config
ENV
chmod 0644 "$FUSIONGATE_HOME/config/compose.env"
printf '%s\n' "$REPOSITORY@$REF" > "$FUSIONGATE_HOME/.fusiongate-install"

log "Building and starting production services"
deployment_started=true
compose_release up -d --build --remove-orphans || deployment_started=false

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
    docker image rm "$rollback_app_image" >/dev/null 2>&1 || true
  fi
  remove_legacy_command
  docker builder prune --force --filter 'until=168h' --keep-storage 5GB >/dev/null 2>&1 || true
  log "FusionGate is online: https://$FUSIONGATE_DOMAIN"
else
  if $UPDATE_ONLY; then
    if rollback_update; then
      rollback_pending=false
      die "Deployment failed readiness checks. The previous release was restored"
    fi
    rollback_pending=false
    die "Deployment and automatic rollback failed. Inspect: $COMPOSE_HINT logs"
  fi
  die "Deployment failed readiness checks. Inspect: $COMPOSE_HINT logs"
fi
log "Useful commands: $FUSIONGATE_HOME/app/deploy/install.sh --update | --backup | --restore FILE"
log "Status and logs: $COMPOSE_HINT ps | logs"
