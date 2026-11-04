#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd -P)
cd "$ROOT"
compose() { docker compose --env-file .env.test -f compose.test.yml "$@"; }
case "${1:-help}" in
  init)
    [ ! -e .env.test ] || { echo '.env.test already exists; keeping it.'; exit 1; }
    read -r -s -p 'Independent TEST Bot token: ' token; echo
    read -r -p 'TEST Bot username (without @): ' username
    read -r -p 'Your Telegram numeric user ID: ' admin
    [[ "$token" =~ ^[0-9]+:[A-Za-z0-9_-]+$ && "$username" =~ ^[A-Za-z0-9_]+$ && "$admin" =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid token, username or user ID.' >&2; exit 1; }
    key=$(openssl rand -hex 32); pepper=$(openssl rand -hex 32)
    umask 077
    (set -o noclobber; cat > .env.test <<EOF
APP_ENV=test
ABS_MODE=mock
TELEGRAM_BOT_TOKEN=$token
TEST_BOT_USERNAME=$username
ADMIN_TELEGRAM_IDS=$admin
BACKUP_ENCRYPT_KEY=$key
SECURITY_PEPPER=$pepper
TEST_DATA_DIR=data-test
DATABASE_URL=data-test/bot_data.db
NOTICE_GROUP_ID=0
BACKUP_GROUP_ID=0
INVITE_REQUIRED=false
WORKER_COUNT=4
QUEUE_CAPACITY=1000
DATABASE_MAX_OPEN_CONNS=1
DATABASE_MAX_IDLE_CONNS=1
EOF
    )
    echo 'Created .env.test. Run ./scripts/test-bot.sh run (Go source) or up (Docker), then open your test Bot.'
    ;;
  run)
    [ -f .env.test ] || { echo 'Run ./scripts/test-bot.sh init first.' >&2; exit 1; }
    if command -v go >/dev/null 2>&1; then
      go_cmd=$(command -v go)
    elif [ -x "$ROOT/.tools/go/bin/go" ]; then
      go_cmd="$ROOT/.tools/go/bin/go"
    else
      echo 'Go is required for native startup. Install Go 1.26.10 and Xcode Command Line Tools (CGO/SQLite).' >&2
      exit 1
    fi
    export CGO_ENABLED=1 GOFLAGS=-mod=readonly
    echo 'Starting test Bot from source (.env.test + mock ABS). Ctrl+C stops it.'
    exec "$go_cmd" run . test-bot
    ;;
  up)
    [ -f .env.test ] || { echo 'Run ./scripts/test-bot.sh init first.' >&2; exit 1; }
    if [ "${2:-local}" = local ]; then
      unset TEST_BOT_IMAGE
      compose up -d --build
    else
      [[ "$2" =~ ^sha-[a-f0-9]{40}$ ]] || { echo 'Use local or sha-FULL_COMMIT_HASH.' >&2; exit 1; }
      export TEST_BOT_IMAGE="${IMAGE_REPOSITORY:-ghcr.io/ccoffx/ccoff_abs_bot}:$2"
      compose up -d --no-build --pull always
    fi
    ;;
  down) compose down ;;
  logs) compose logs -f --tail 100 ;;
  status) compose ps ;;
  reset|reset-native)
    [ "${2:-}" = --confirm ] || { echo 'Deletes ALL test data: reset --confirm (Docker) or reset-native --confirm. Keeps .env.test and its keys.' >&2; exit 1; }
    [ -f data-test/.test-environment ] && [ ! -L data-test ] || { echo 'Refusing unmarked or symlinked data-test.' >&2; exit 1; }
    if [ "$1" = reset-native ]; then
      command -v lsof >/dev/null 2>&1 || { echo 'lsof is required to check that the native test Bot is stopped.' >&2; exit 1; }
      scan_error=$(mktemp)
      open_pids=$(lsof -t +D "$ROOT/data-test" 2> "$scan_error") || true
      if [ -s "$scan_error" ]; then
        cat "$scan_error" >&2; rm -f "$scan_error"
        echo 'Could not verify that the test data is unused; nothing was deleted.' >&2
        exit 1
      fi
      rm -f "$scan_error"
      [ -z "$open_pids" ] || { echo 'Stop the native test Bot before resetting its data.' >&2; exit 1; }
      find "$ROOT/data-test" -mindepth 1 -maxdepth 1 ! -name .test-environment -exec rm -rf -- {} +
      echo 'Native test data cleared. Start the test Bot again.'
      exit 0
    fi
    image=$(compose images -q bot)
    [ -n "$image" ] || { echo 'No test image found. Start the test Bot before resetting.' >&2; exit 1; }
    compose down
    # Run with the image's filesystem tools; Docker may have assigned UID 10001.
    docker run --rm --network none --user 0:0 --entrypoint /bin/sh --mount "type=bind,src=$ROOT/data-test,dst=/reset" "$image" -c 'find /reset -mindepth 1 -maxdepth 1 ! -name .test-environment -exec rm -rf {} +'
    echo 'Test data cleared. Start the test Bot again.'
    ;;
  *) echo 'Usage: ./scripts/test-bot.sh init|run (native Go)|up [local|sha-FULL_HASH] (Docker)|down|logs|status|reset --confirm (Docker)|reset-native --confirm' ;;
esac
