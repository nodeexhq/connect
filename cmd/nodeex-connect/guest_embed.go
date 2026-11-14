//go:build guestbin

package main

import _ "embed"

// guestBinary is nodeex-guest for macOS on Apple silicon, built by
// scripts/build-guest.sh before a release build with -tags guestbin.
//
//go:embed guestbin/nodeex-guest
var guestBinary []byte
