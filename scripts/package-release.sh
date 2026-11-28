#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
tar -czf dist/deployment.tar.gz deploy.sh docker-compose.yml .env.example docs/deployment.md scripts/audiobookshelf/listening-days.cjs
(cd dist && shasum -a 256 deployment.tar.gz > deployment.tar.gz.sha256)
