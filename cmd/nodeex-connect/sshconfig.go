package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// While a lease is open, Connect gives its machine a name in the user's SSH
// configuration, so that `ssh nodeex-<lease>` works and so do the editors
// that connect over SSH (VS Code and Cursor Remote-SSH, JetBrains Gateway).
// Each lease gets its own file under ~/.ssh/nodeex/, written when the
// machine is ready and removed when Connect disconnects; ~/.ssh/config gets
// one line at its top that includes them.

const sshInclude = "Include nodeex/*.conf"

// sshHost is the name a lease's machine goes by in SSH.
func sshHost(leaseID string) string {
	id := leaseID
	if len(id) > 8 {
		id = id[:8]
	}
	return "nodeex-" + id
}

func sshConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "nodeex"), nil
}

// sshQuote wraps a value for an ssh_config line when it holds a space.
func sshQuote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// writeSSHHost writes the lease's host entry and makes sure ~/.ssh/config
// includes the entries. It returns the host name.
func writeSSHHost(stateDir, leaseID string, port int) (string, error) {
	dir, err := sshConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	host := sshHost(leaseID)
	known := filepath.Join(dir, "known_hosts-"+strings.TrimPrefix(host, "nodeex-"))
	entry := fmt.Sprintf(`# Written by NodeEx Connect for lease %s while it is connected;
# removed when Connect disconnects.
Host %s
  HostName 127.0.0.1
  Port %d
  User %s
  IdentityFile %s
  IdentitiesOnly yes
  StrictHostKeyChecking accept-new
  UserKnownHostsFile %s
  ServerAliveInterval 30
  ServerAliveCountMax 4
  Compression yes
`, leaseID, host, port, machineAccount, sshQuote(sshKeyPath(stateDir)), sshQuote(known))
	if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(host, "nodeex-")+".conf"), []byte(entry), 0o600); err != nil {
		return "", err
	}
	return host, ensureSSHInclude(filepath.Dir(dir))
}

// ensureSSHInclude puts the Include line at the top of ~/.ssh/config, once.
// It must come before any Host block to apply everywhere.
func ensureSSHInclude(sshDir string) error {
	path := filepath.Join(sshDir, "config")
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(old), "\n") {
		if strings.TrimSpace(line) == sshInclude {
			return nil
		}
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	text := "# Added by NodeEx Connect: names for rented machines while connected.\n" + sshInclude + "\n\n" + string(old)
	return os.WriteFile(path, []byte(text), mode)
}

// removeSSHHost deletes the lease's entry; the Include line stays.
func removeSSHHost(leaseID string) {
	dir, err := sshConfigDir()
	if err != nil {
		return
	}
	id := strings.TrimPrefix(sshHost(leaseID), "nodeex-")
	os.Remove(filepath.Join(dir, id+".conf"))
}
