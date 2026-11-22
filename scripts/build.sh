#!/usr/bin/env bash
# Builds Pendrive Rescue for Windows (64 and 32 bit) and Linux into dist/,
# the same way every time: no C compiler, no build paths, no debug data.
set -euo pipefail
cd "$(dirname "$0")/.."
export CGO_ENABLED=0
flags=(-trimpath -ldflags "-s -w")
mkdir -p dist
GOOS=windows GOARCH=amd64 go build "${flags[@]}" -o dist/PendriveRescue.exe .
GOOS=windows GOARCH=386 go build "${flags[@]}" -o dist/PendriveRescue-32bit.exe .
GOOS=linux GOARCH=amd64 go build "${flags[@]}" -o dist/pendrive-rescue-linux .
cp README.md dist/
cp "internal/webui/static/fonts/OFL.txt" "dist/Font licence (OFL).txt"
( cd dist && sha256sum PendriveRescue.exe PendriveRescue-32bit.exe pendrive-rescue-linux > SHA256SUMS.txt )
echo "Built into dist/:"
ls -l dist
