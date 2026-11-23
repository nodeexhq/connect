#!/bin/sh
# Builds nvto-guest, the measuring program Connect copies into a rented
# machine, where Connect embeds it. Build Connect afterwards with
# -tags guestbin.
set -eu
cd "$(dirname "$0")/.."
mkdir -p cmd/nvto-connect/guestbin
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" \
  -o cmd/nvto-connect/guestbin/nvto-guest ./cmd/nvto-guest
