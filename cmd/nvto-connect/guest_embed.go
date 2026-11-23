//go:build guestbin

package main

import _ "embed"

// guestBinary is nvto-guest for macOS on Apple silicon, built by
// scripts/build-guest.sh before a release build with -tags guestbin.
//
//go:embed guestbin/nvto-guest
var guestBinary []byte
