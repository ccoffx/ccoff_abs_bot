#!/usr/bin/env bash
# Deployment bundle: Bash + Docker Compose only; never source the secrets file.
set -euo pipefail
umask 077
ROOT=$(cd "$(dirname "$0")" && pwd)
cd "$ROOT"
STATE="$ROOT/deploy-state"
REPOSITORY=${IMAGE_REPOSITORY:-ghcr.io/huliyoudiangou/yueer_abs_bot}
PLATFORM=linux/amd64
WAIT_SECONDS=${DEPLOY_WAIT_SECONDS:-180}
[[ "$WAIT_SECONDS" =~ ^[0-9]+$ ]] && [ "$WAIT_SECONDS" -le 180 ] || { echo 'DEPLOY_WAIT_SECONDS must be 0..180.' >&2; exit 1; }
die() { echo "$*" >&2; exit 1; }
compose() { docker compose --env-file "$ROOT/.release.env" -f "$ROOT/docker-compose.yml" "$@"; }
current_image() { [ ! -f .release.env ] || sed -n 's/^BOT_IMAGE=//p' .release.env; }
check_pin() { [[ "$1" =~ ^[a-z0-9./:_-]+@sha256:[a-f0-9]{64}$ ]] || die 'Invalid pinned image.'; }
version_ok() { [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'Use a version such as v1.0.1; latest is not accepted.'; }

# Maintenance runs offline as root to read a host-owned 0600 .env and restore
# app-owned SQLite files. Backups are returned to the invoking host user's UID.
maintenance() {
  local image=$1; shift
  docker run --rm --platform "$PLATFORM" --network none --read-only --tmpfs /tmp \
    --user 0:0 --security-opt no-new-privileges:true \
    --mount "type=bind,src=$ROOT/data,dst=/app/data" \
    --mount "type=bind,src=$ROOT/.env,dst=/app/.env,readonly" \
    --mount "type=bind,src=$STATE/backups,dst=/backups" \
    -e "BACKUP_OWNER=$(id -u):$(id -g)" --entrypoint /bin/sh "$image" \
    -c '/app/ccoff-abs-bot "$@"; result=$?; chown "$BACKUP_OWNER" /backups/*.enc 2>/dev/null || true; exit "$result"' sh maintenance "$@"
}
schema() { docker run --rm --platform "$PLATFORM" --network none --read-only --user 10001:10001 --entrypoint /app/ccoff-abs-bot "$1" version --schema; }
copy_backup() {
  local file=$1 destination
  if [ -f "$STATE/backup-destination.txt" ]; then
    IFS= read -r destination < "$STATE/backup-destination.txt"
    [ -d "$destination" ] && [ -w "$destination" ] || die 'External backup destination is unavailable; local backup retained.'
    cp -p "$STATE/backups/$file" "$destination/$file"
  fi
}
backup_now() {
  local image=$1 kind=$2 file
  file="$(date -u +%Y%m%dT%H%M%SZ)-${kind}-$$.enc"
  maintenance "$image" backup "/backups/$file"
  printf '%s\n' "$file" > "$STATE/last-backup.txt"
  copy_backup "$file"
  echo "Backup verified: deploy-state/backups/$file"
}
wait_healthy() {
  local elapsed=0 id status
  while [ "$elapsed" -le "$WAIT_SECONDS" ]; do
    id=$(compose ps -a -q ccoff_abs_bot)
    if [ -n "$id" ]; then
      status=$(docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$id")
      if [ "$status" = 'running healthy' ]; then return 0; fi
    fi
    [ "$elapsed" -lt "$WAIT_SECONDS" ] || break
    sleep 3; elapsed=$((elapsed+3))
  done
  return 1
}
record_failure() {
  local file="$STATE/failed-$(date -u +%Y%m%dT%H%M%SZ)-$$.log"
  compose logs --no-color --tail 200 > "$file" 2>&1 || true
  # Leave the new instance stopped; no hidden retries against the database.
  compose stop ccoff_abs_bot || true
  printf '%s\tfailed\n' "$(date -u +%FT%TZ)" >> "$STATE/history.tsv"
  echo "Update failed. New instance stopped; database was NOT restored. Logs: $file" >&2
  echo 'Use status; rollback a previously successful compatible version, or restore with --confirm-data-loss.' >&2
}

command=${1:-help}
case "$command" in
  help|--help|-h)
    echo 'Usage: ./deploy.sh update VERSION | rollback VERSION | status | logs | backup | restore BACKUP_FILENAME --confirm-data-loss | set-backup-destination DIRECTORY'
    exit 0 ;;
esac
command -v docker >/dev/null 2>&1 || die 'Docker is required.'
docker compose version >/dev/null
mkdir -p "$STATE/backups" "$STATE/releases" "$ROOT/data"
case "$command" in
  status)
    [ -f .release.env ] || die 'No release selected yet.'
    cat .release.env
    compose ps -a
    [ ! -f "$STATE/last-backup.txt" ] || { printf 'Last backup: '; cat "$STATE/last-backup.txt"; }
    exit 0 ;;
  logs) [ -f .release.env ] || die 'No release selected yet.'; compose logs -f --tail 100; exit 0 ;;
esac
[ -f .env ] || die 'Copy .env.example to .env and configure production credentials first.'
mkdir "$STATE/.lock" 2>/dev/null || die 'Another deployment/backup is running (or a stale deploy-state/.lock needs inspection).'
trap 'rmdir "$STATE/.lock"' EXIT
case "$command" in
  set-backup-destination)
    [ "$#" -eq 2 ] && [ -d "$2" ] && [ -w "$2" ] || die 'Supply an existing writable external backup directory.'
    destination=$(cd "$2" && pwd)
    case "$destination/" in "$ROOT/"*) die 'Choose a directory outside the deployment directory.';; esac
    printf '%s\n' "$destination" > "$STATE/backup-destination.txt"
    echo 'Backup copy destination saved. Keep a separate secure copy of your encryption key.' ;;
  update|rollback)
    [ "$#" -eq 2 ] || die 'Supply exactly one version.'
    version_ok "$2"; version=$2
    if [ "$command" = rollback ]; then
      [ -f "$STATE/releases/$version.env" ] || die 'Rollback target must be a previously successful local deployment.'
      pin=$(sed -n 's/^BOT_IMAGE=//p' "$STATE/releases/$version.env")
      check_pin "$pin"
      # A registry outage must not prevent rollback to a cached good image.
      if ! docker image inspect "$pin" >/dev/null 2>&1; then docker pull --platform "$PLATFORM" "$pin"; fi
    else
      ref="$REPOSITORY:$version"
      docker pull --platform "$PLATFORM" "$ref"
      pin=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref" | awk -v prefix="$REPOSITORY@sha256:" 'index($0,prefix)==1 {print; exit}')
      check_pin "$pin"
      if [ -f "$STATE/releases/$version.env" ]; then
        [ "$pin" = "$(sed -n 's/^BOT_IMAGE=//p' "$STATE/releases/$version.env")" ] || die 'Version tag changed digest; refusing replacement.'
      fi
    fi
    old=$(current_image)
    if [ -n "$old" ]; then
      check_pin "$old"
      [ "$(schema "$old")" = "$(schema "$pin")" ] || die 'Database compatibility differs; a separate migration procedure is required.'
    fi
    # Keep enough room for a SQLite snapshot, encryption buffers/files and WAL.
    required=$(( $(du -sk data | awk '{print $1}') * 4 + 102400 ))
    available=$(df -Pk "$ROOT" | awk 'END {print $4}')
    [ "$available" -ge "$required" ] || die 'Insufficient free space for a verified backup.'
    maintenance "$pin" preflight
    if [ -n "$old" ]; then compose stop ccoff_abs_bot; fi
    if [ -f data/bot_data.db ]; then
      # Backup failure occurs before selecting the new image; keep the old
      # stopped rather than starting it implicitly after an uncertain failure.
      backup_now "$pin" "before-$command-$version"
    fi
    [ ! -f .release.env ] || cp .release.env "$STATE/previous-release.env"
    printf 'BOT_IMAGE=%s\nBOT_VERSION=%s\n' "$pin" "$version" > "$STATE/pending-release.env"
    mv "$STATE/pending-release.env" .release.env
    docker run --rm --platform "$PLATFORM" --network none --read-only --entrypoint /app/ccoff-abs-bot "$pin" version > "$STATE/releases/$version.json"
    printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$command-started" "$version" "$pin" >> "$STATE/history.tsv"
    if compose up -d --no-build --pull never ccoff_abs_bot && wait_healthy; then
      cp .release.env "$STATE/releases/$version.env"
      printf '%s\tsuccess\t%s\t%s\n' "$(date -u +%FT%TZ)" "$version" "$pin" >> "$STATE/history.tsv"
      echo "Healthy: $version ($pin)"
    else
      record_failure; exit 1
    fi ;;
  backup)
    image=$(current_image); [ -n "$image" ] || die 'Deploy a release first.'; check_pin "$image"
    # Online VACUUM INTO is consistent; no daily Bot downtime is required.
    backup_now "$image" daily ;;
  restore)
    [ "$#" -eq 3 ] && [ "$3" = --confirm-data-loss ] || die 'restore BACKUP_FILENAME --confirm-data-loss; new data since that backup will be lost.'
    [[ "$2" =~ ^[a-zA-Z0-9._-]+\.enc$ ]] && [ -f "$STATE/backups/$2" ] || die 'Choose a backup filename from deploy-state/backups.'
    image=$(current_image); [ -n "$image" ] || die 'Select a compatible release first.'; check_pin "$image"
    compose stop ccoff_abs_bot
    [ ! -f data/bot_data.db ] || backup_now "$image" before-restore
    maintenance "$image" restore "/backups/$2" --confirm-data-loss
    if compose up -d --no-build --pull never ccoff_abs_bot && wait_healthy; then echo 'Database restored; Bot healthy.'; else record_failure; exit 1; fi ;;
  *) die "Unknown command: $command" ;;
esac
