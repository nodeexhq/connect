package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// sshKeyPath is where Connect keeps the key it logs in to rented machines
// with. It is this computer's own: the private half never leaves it.
func sshKeyPath(dir string) string { return filepath.Join(dir, "id_ed25519") }

// ensureSSHKey returns the public key in authorized_keys form, creating the
// pair on first use. The private key is written in OpenSSH's format so that
// plain `ssh -i` can use it.
func ensureSSHKey(dir string) (string, error) {
	path := sshKeyPath(dir)
	if b, err := os.ReadFile(path + ".pub"); err == nil {
		if _, err := os.Stat(path); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "nvto-connect")
	if err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " nvto-connect"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".pub", []byte(line+"\n"), 0o644); err != nil {
		return "", err
	}
	return line, nil
}
