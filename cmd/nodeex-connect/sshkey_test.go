package main

import (
	"os"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestEnsureSSHKey(t *testing.T) {
	dir := t.TempDir()
	pub, err := ensureSSHKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ensureSSHKey(dir)
	if err != nil || again != pub {
		t.Fatalf("second call gave %q, %v; want the same key", again, err)
	}
	parsed, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil || comment != "nodeex-connect" || parsed.Type() != "ssh-ed25519" {
		t.Fatalf("public key %q: %v", pub, err)
	}
	priv, err := os.ReadFile(sshKeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("private key is not usable: %v", err)
	}
	if string(signer.PublicKey().Marshal()) != string(parsed.Marshal()) {
		t.Error("the two halves do not belong together")
	}
	if info, _ := os.Stat(sshKeyPath(dir)); info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode %v, want 0600", info.Mode().Perm())
	}
}

func TestNewPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := newPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 23 || p[5] != '-' || p[11] != '-' || p[17] != '-' {
			t.Fatalf("password %q is not four groups of five", p)
		}
		for _, r := range p {
			if r == '0' || r == 'o' || r == '1' || r == 'l' || r == 'i' {
				t.Fatalf("password %q has a character that is easy to misread", p)
			}
		}
		if seen[p] {
			t.Fatalf("password %q came up twice", p)
		}
		seen[p] = true
	}
}
