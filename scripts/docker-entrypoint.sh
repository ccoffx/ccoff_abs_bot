#!/bin/sh
set -eu

runtime_data=/app/data
if [ "${APP_ENV:-production}" = "test" ]; then runtime_data=/app/data-test; fi
mkdir -p "$runtime_data"

if [ "$(id -u)" = "0" ]; then
	if ! chown -R app:app "$runtime_data"; then
		echo "WARN: failed to chown /app/data; continuing as app, SQLite startup may fail if the directory is not writable" >&2
	fi
	exec su-exec app "$@"
fi

exec "$@"
