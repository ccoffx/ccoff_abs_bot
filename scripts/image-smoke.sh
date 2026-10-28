#!/usr/bin/env bash
set -euo pipefail
image=$1
platform=${2:-linux/amd64}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
python3 - "$work" <<'PY'
import pathlib, sqlite3, sys
root = pathlib.Path(sys.argv[1])
with sqlite3.connect(root / 'bot.db') as db:
    db.executescript("CREATE TABLE deployment_metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL); INSERT INTO deployment_metadata VALUES('schema_compatibility','1'); CREATE TABLE balances(id INTEGER PRIMARY KEY,points INTEGER); INSERT INTO balances VALUES(1,123);")
PY
run() { docker run --rm --platform "$platform" --network none --read-only --tmpfs /tmp --user "$(id -u):$(id -g)" --mount "type=bind,src=$work,dst=/work" -e DATABASE_URL=/work/bot.db -e BACKUP_ENCRYPT_KEY=0123456789abcdef0123456789abcdef --entrypoint /app/ccoff-abs-bot "$image" "$@"; }
run version
test "$(run version --schema)" = 1
run maintenance check-db
run maintenance backup /work/backup.enc
python3 - "$work/bot.db" <<'PY'
import sqlite3, sys
with sqlite3.connect(sys.argv[1]) as db:
    db.execute('UPDATE balances SET points=999')
PY
run maintenance restore /work/backup.enc --confirm-data-loss
python3 - "$work/bot.db" <<'PY'
import sqlite3, sys
with sqlite3.connect(sys.argv[1]) as db:
    assert db.execute('SELECT points FROM balances').fetchone()[0] == 123
PY
if run maintenance health; then echo 'A maintenance-only container must not be healthy.' >&2; exit 1; fi
