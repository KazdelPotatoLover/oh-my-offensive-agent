#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
python3 scripts/prepare-runtime.py
docker build -f deploy/execution/Dockerfile -t oma-executor:ma4 backend
docker compose -p oh-my-offensive-agent up -d --build --wait --wait-timeout 60
