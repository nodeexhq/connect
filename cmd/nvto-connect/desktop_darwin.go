package main

import (
	"context"
	"fmt"
	"os/exec"
)

// macOS ships a remote desktop client, Screen Sharing, which handles vnc://
// links.

func desktopHint(host string, port int) string {
	return fmt.Sprintf("open vnc://%s:%d", host, port)
}

func openDesktop(ctx context.Context, host string, port int) error {
	return exec.CommandContext(ctx, "open", fmt.Sprintf("vnc://%s:%d", host, port)).Run()
}
