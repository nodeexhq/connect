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
	// PrivatePassphrase locks the machine's encrypted private volume. Like
	// the password, Connect chose it and it is kept here and nowhere else.
	PrivatePassphrase string `json:"private_passphrase,omitempty"`
	// CheckedAt is when this computer last checked the machine.
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

func loadAccess(dir, leaseID string) machineAccess {
	var a machineAccess
	if b, err := os.ReadFile(accessPath(dir, leaseID)); err == nil {
		json.Unmarshal(b, &a)
	}
	return a
}

func saveAccess(dir, leaseID string, a machineAccess) error {
	b, _ := json.Marshal(a)
	if err := os.MkdirAll(filepath.Dir(accessPath(dir, leaseID)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(accessPath(dir, leaseID), b, 0o600)
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
	a := loadAccess(dir, leaseID)
	a.DesktopPassword = password
	return password, saveAccess(dir, leaseID, a)
}

var errNoMachine = errors.New("the machine did not accept this computer's key")

// privateVolume is the encrypted APFS volume Connect makes in a lease's
// machine for the renter's files and keys. Its passphrase exists only on
// the renter's computer: whenever the machine is not running (the node went
// offline and was shut down, was restarted, or its disk was copied) the
// volume is locked and its contents are unreadable, the owner's included.
// While the machine runs, the volume stays unlocked; an owner set on it
// could reach it then, which the security page says plainly.
const privateVolume = "NodeEx Private"

// privateLink is where the renter finds the volume in their home folder.
const privateLink = "Private"

type apfsList struct {
	Containers []struct {
		ContainerReference string `json:"ContainerReference"`
		Volumes            []struct {
			Name             string `json:"Name"`
			DeviceIdentifier string `json:"DeviceIdentifier"`
			Locked           bool   `json:"Locked"`
		} `json:"Volumes"`
	} `json:"Containers"`
}

func runOn(client *ssh.Client, command, input string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	if input != "" {
		session.Stdin = strings.NewReader(input)
	}
	out, err := session.CombinedOutput(command)
	return string(out), err
}

// preparePrivateVolume makes the lease's private volume on first use and
// unlocks it on later connections, and returns where it is mounted. The
// passphrase is saved before the volume is made, so a volume never exists
// whose passphrase was lost.
func preparePrivateVolume(ctx context.Context, dir, leaseID string, port int) (string, error) {
	client, err := machineLogin(ctx, dir, port)
	if err != nil {
		return "", err
	}
	defer client.Close()
	a := loadAccess(dir, leaseID)
	if a.PrivatePassphrase == "" {
		p, err := newPassword()
		if err != nil {
			return "", err
		}
		a.PrivatePassphrase = p
		if err := saveAccess(dir, leaseID, a); err != nil {
			return "", err
		}
	}
	out, err := runOn(client, `/usr/sbin/diskutil apfs list -plist | /usr/bin/plutil -convert json -o - -`, "")
	if err != nil {
		return "", fmt.Errorf("could not list the machine's volumes: %v", err)
	}
	var list apfsList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return "", errors.New("could not read the machine's volume list")
	}
	device, locked := "", false
	for _, c := range list.Containers {
		for _, v := range c.Volumes {
			if v.Name == privateVolume {
				device, locked = v.DeviceIdentifier, v.Locked
			}
		}
	}
	mount := "/Volumes/" + privateVolume
	// The passphrase travels on standard input, never on a command line.
	switch {
	case device == "":
		ref, err := runOn(client, `/usr/sbin/diskutil info -plist / | /usr/bin/plutil -extract APFSContainerReference raw -`, "")
		ref = strings.TrimSpace(ref)
		if err != nil || !validDisk(ref) {
			return "", fmt.Errorf("could not find the machine's disk: %v %s", err, ref)
		}
		if out, err := runOn(client, `IFS= read -r p && printf '%s\n' "$p" | sudo -n /usr/sbin/diskutil apfs addVolume `+ref+` APFS "`+privateVolume+`" -stdinpassphrase`, a.PrivatePassphrase+"\n"); err != nil {
			return "", fmt.Errorf("could not make the private volume: %v %s", err, lastLine(out))
		}
	case locked:
		if !validDisk(device) {
			return "", fmt.Errorf("unexpected volume %q", device)
		}
		if out, err := runOn(client, `IFS= read -r p && printf '%s\n' "$p" | sudo -n /usr/sbin/diskutil apfs unlockVolume `+device+` -stdinpassphrase`, a.PrivatePassphrase+"\n"); err != nil {
			return "", fmt.Errorf("could not unlock the private volume: %v %s", err, lastLine(out))
		}
	}
	// Files on it belong to the machine account, and it shows up as
	// ~/Private.
	setup := `sudo -n /usr/sbin/diskutil enableOwnership "` + mount + `" >/dev/null 2>&1; ` +
		`sudo -n /usr/sbin/chown ` + machineAccount + `:staff "` + mount + `" && ` +
		`{ [ -e "$HOME/` + privateLink + `" ] || /bin/ln -s "` + mount + `" "$HOME/` + privateLink + `"; }`
	if out, err := runOn(client, setup, ""); err != nil {
		return "", fmt.Errorf("could not set up the private volume: %v %s", err, lastLine(out))
	}
	return "~/" + privateLink, nil
}

// reopenPrivateVolume unlocks the private volume again if the machine has
// lost it, which is what a restart does. again is false when the volume is
// in place or the machine cannot be reached just now, which is not worth a
// word: a paused machine answers nothing, and comes back as it was.
func reopenPrivateVolume(ctx context.Context, dir, leaseID string, port int) (again bool, err error) {
	client, err := machineLogin(ctx, dir, port)
	if err != nil {
		return false, nil
	}
	out, err := runOn(client, `[ -d "/Volumes/`+privateVolume+`" ] && echo mounted || echo missing`, "")
	client.Close()
	if err != nil || strings.TrimSpace(out) != "missing" {
		return false, nil
	}
	_, err = preparePrivateVolume(ctx, dir, leaseID, port)
	return true, err
}

// validDisk accepts a disk identifier such as disk3 or disk3s7, and
// nothing that could change the meaning of the command it goes into.
func validDisk(s string) bool {
	if !strings.HasPrefix(s, "disk") || len(s) > 16 {
		return false
	}
	for _, c := range s[4:] {
		if !(c >= '0' && c <= '9' || c == 's') {
			return false
		}
	}
	return len(s) > 4
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
