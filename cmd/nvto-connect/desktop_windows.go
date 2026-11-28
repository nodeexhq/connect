package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows/registry"
)

// Windows has no VNC client of its own. One has to be installed, and only
// some of them register themselves for vnc:// links.

func desktopHint(host string, port int) string {
	return fmt.Sprintf("connect a VNC viewer (for example RealVNC Viewer) to %s:%d", host, port)
}

// openDesktop opens the desktop in the viewer registered for vnc:// links.
// The viewers differ in whether they take a password from the link, so it
// is not put there: Connect shows it instead.
func openDesktop(ctx context.Context, host string, port int, password string) error {
	// Without a registered handler Windows would only ask which app to
	// use, which helps nobody: say what is missing instead.
	k, err := registry.OpenKey(registry.CLASSES_ROOT, `vnc\shell\open\command`, registry.QUERY_VALUE)
	if err != nil {
		return errors.New("no VNC viewer is registered for vnc:// links; install one, or open it yourself and " + desktopHint(host, port))
	}
	k.Close()
	return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", fmt.Sprintf("vnc://%s:%d", host, port)).Run()
}
