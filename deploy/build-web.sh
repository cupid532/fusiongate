#!/usr/bin/env bash
# Build the React console and stage its output into the Go embed directory.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/web"
npm run build
cd "$ROOT"
rm -rf internal/fusiongate/ui/*
cp -r web/dist/* internal/fusiongate/ui/
echo "web build staged into internal/fusiongate/ui"
