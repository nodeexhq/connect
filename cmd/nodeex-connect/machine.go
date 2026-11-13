package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// machineAccount is the account in a machine that a node creates for a
// lease. A node that forwards to an existing system instead has whatever
// accounts its owner made, and Connect cannot know them.
const machineAccount = "renter"

// A lease's machine as this computer knows it.
type machineAccess struct {
	// DesktopPassword is the account's password, which remote desktop asks
	// for. Connect chose it and set it in the machine; it is kept here and
	// nowhere else.
	DesktopPassword string `json:"desktop_password"`
}

func accessPath(dir, leaseID string) string { return filepath.Join(dir, "leases", leaseID+".json") }

// newPassword is a password a person can read aloud and type: twenty
// characters without the ones that are easy to confuse.
func newPassword() (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, r := range raw {
		if i > 0 && i%5 == 0 {
			b.WriteByte('-')
		}
		// 31 symbols: the slight bias of the modulo is of no account here.
		b.WriteByte(alphabet[int(r)%len(alphabet)])
	}
	return b.String(), nil
}

// machineLogin opens an SSH session to the lease's machine through the
// local port, as the machine account with this computer's key.
//
// The host key is not checked: every lease gets a newly made machine, so
// there is no earlier key to compare with, and the connection already runs
// inside the overlay to a gate that admitted this lease's token.
func machineLogin(ctx context.Context, dir string, port int) (*ssh.Client, error) {
	pem, err := os.ReadFile(sshKeyPath(dir))
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            machineAccount,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         8 * time.Second,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// prepareMachine waits until the lease's machine lets this computer's key
// in, then makes sure the account has a password for remote desktop. It
// returns that password. errNoMachine means the node never accepted the
// key: it does not give leases a machine of their own.
func prepareMachine(ctx context.Context, dir, leaseID string, port int, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	var client *ssh.Client
	for {
		c, err := machineLogin(ctx, dir, port)
		if err == nil {
			client = c
			break
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if time.Now().After(deadline) {
			return "", errNoMachine
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	defer client.Close()

	var access machineAccess
	path := accessPath(dir, leaseID)
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &access) == nil && access.DesktopPassword != "" {
		return access.DesktopPassword, nil
	}
	password, err := newPassword()
	if err != nil {
		return "", err
	}
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	// The password travels on standard input, not on a command line.
	session.Stdin = strings.NewReader(password + "\n")
	const set = `read -r p && sudo -n /usr/bin/dscl . -passwd /Users/` + machineAccount + ` "$p"`
	if out, err := session.CombinedOutput(set); err != nil {
		return "", fmt.Errorf("could not set the machine's password: %v %s", err, strings.TrimSpace(string(out)))
	}
	b, _ := json.Marshal(machineAccess{DesktopPassword: password})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return password, err
	}
	return password, os.WriteFile(path, b, 0o600)
}

var errNoMachine = errors.New("the machine did not accept this computer's key")
