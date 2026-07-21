#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
echo "Building api-gateway for Linux..."
cd src
go build -ldflags="-s -w" -o ../api-gateway .
echo "Done: api-gateway"
