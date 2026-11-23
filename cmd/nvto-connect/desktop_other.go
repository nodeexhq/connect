//go:build !darwin && !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// On Linux and the BSDs the desktop's own viewer (GNOME Connections,
// Remmina, KRDC) usually handles vnc:// links through xdg-open.

func desktopHint(host string, port int) string {
	return fmt.Sprintf("connect a VNC viewer (for example Remmina) to %s:%d, or run: xdg-open vnc://%s:%d", host, port, host, port)
}

func openDesktop(ctx context.Context, host string, port int) error {
	path, err := exec.LookPath("xdg-open")
	if err != nil {
		return errors.New("xdg-open is not installed; " + desktopHint(host, port))
	}
	return exec.CommandContext(ctx, path, fmt.Sprintf("vnc://%s:%d", host, port)).Run()
}
