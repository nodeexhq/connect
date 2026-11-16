package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHHostEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	os.MkdirAll(sshDir, 0o700)
	existing := "Host work\n  HostName example.com\n"
	os.WriteFile(filepath.Join(sshDir, "config"), []byte(existing), 0o644)

	state := filepath.Join(home, "Library", "Application Support", "NodeEx")
	host, err := writeSSHHost(state, "3f9a1c2e-1111-2222-3333-444455556666", 10022)
	if err != nil || host != "nodeex-3f9a1c2e" {
		t.Fatalf("host %q, %v", host, err)
	}
	conf, _ := os.ReadFile(filepath.Join(sshDir, "config"))
	if !strings.HasPrefix(string(conf), "# Added by NodeEx Connect") || !strings.Contains(string(conf), sshInclude+"\n") || !strings.HasSuffix(string(conf), existing) {
		t.Errorf("config:\n%s", conf)
	}
	if st, _ := os.Stat(filepath.Join(sshDir, "config")); st.Mode().Perm() != 0o644 {
		t.Errorf("config mode changed to %v", st.Mode().Perm())
	}
	entry, _ := os.ReadFile(filepath.Join(sshDir, "nodeex", "3f9a1c2e.conf"))
	for _, want := range []string{"Host nodeex-3f9a1c2e", "Port 10022", "User renter", `IdentityFile "` + state + `/id_ed25519"`, "IdentitiesOnly yes"} {
		if !strings.Contains(string(entry), want) {
			t.Errorf("entry lacks %q:\n%s", want, entry)
		}
	}
	// A second lease does not add the Include again.
	writeSSHHost(state, "aaaaaaaa-0000", 10023)
	conf, _ = os.ReadFile(filepath.Join(sshDir, "config"))
	if strings.Count(string(conf), sshInclude) != 1 {
		t.Errorf("Include added twice:\n%s", conf)
	}
	removeSSHHost("3f9a1c2e-1111-2222-3333-444455556666")
	if _, err := os.Stat(filepath.Join(sshDir, "nodeex", "3f9a1c2e.conf")); !os.IsNotExist(err) {
		t.Error("entry left behind after disconnect")
	}
}
