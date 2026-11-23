//go:build !guestbin

package main

// Built without -tags guestbin (a development build), Connect has no
// measuring program to copy into the machine and checks it with the
// machine's own tools instead, which proves less.
var guestBinary []byte
