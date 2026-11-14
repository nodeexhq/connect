#!/bin/sh
# Builds nodeex-guest, the measuring program Connect copies into a rented
# machine, where Connect embeds it. Build Connect afterwards with
# -tags guestbin.
set -eu
cd "$(dirname "$0")/.."
mkdir -p cmd/nodeex-connect/guestbin
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" \
  -o cmd/nodeex-connect/guestbin/nodeex-guest ./cmd/nodeex-guest
