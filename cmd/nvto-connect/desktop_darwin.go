package main

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
)

// macOS ships a remote desktop client, Screen Sharing, which handles vnc://
// links.

func desktopHint(host string, port int) string {
	return fmt.Sprintf("open vnc://%s:%d", host, port)
}

// desktopURL is the vnc:// link for the desktop. With the password of a
// desktop the node serves, Screen Sharing signs in without asking; that
// server checks only a password, so the link names no account.
func desktopURL(host string, port int, password string) string {
	u := url.URL{Scheme: "vnc", Host: fmt.Sprintf("%s:%d", host, port)}
	if password != "" {
		u.User = url.UserPassword("", password)
	}
	return u.String()
}

func openDesktop(ctx context.Context, host string, port int, password string) error {
	return exec.CommandContext(ctx, "open", desktopURL(host, port, password)).Run()
}
