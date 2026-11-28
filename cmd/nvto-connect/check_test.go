package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParseInfo(t *testing.T) {
	out := "VirtualMac2,1\nApple M4 (Virtual)\n8\n12884901888\n/dev/disk3s1  136718748  20400000  110000000  16%  1  2  0%  /System/Volumes/Data\n"
	r, err := parseInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "VirtualMac2,1" || r.CPU != "Apple M4 (Virtual)" || r.Cores != 8 || r.MemoryBytes != 12<<30 ||
		r.DiskBytes != 136718748*1024 || r.DiskFreeBytes != 110000000*1024 {
		t.Fatalf("%+v", r)
	}
	if _, err := parseInfo("nonsense"); err == nil {
		t.Fatal("nonsense parsed")
	}
}

// The commands themselves, run here on a Mac as the machine would run them.
func TestCheckCommandsOnThisMac(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	out, err := exec.Command("sh", "-c", checkInfo).Output()
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseInfo(string(out))
	if err != nil || r.Cores < 1 || r.MemoryBytes < 1<<30 || r.DiskBytes < 1<<30 {
		t.Fatalf("%+v %v\n%s", r, err, out)
	}
	for name, cmd := range map[string]string{"cpu": checkCPU, "disk": checkDisk} {
		start := time.Now()
		if err := exec.Command("sh", "-c", cmd).Run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s: %v", name, time.Since(start))
	}
	t.Logf("%+v", r)
}

// An SSH server that runs each command with sh on this computer, so the
// whole check runs end to end over a real SSH connection.
func shellSSHServer(t *testing.T) *ssh.Client {
	t.Helper()
	return shellSSHServerIn(t, t.TempDir())
}

// shellSSHServerIn is shellSSHServer with home as the account's home.
func shellSSHServerIn(t *testing.T, home string) *ssh.Client {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, creqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer ch.Close()
						for req := range creqs {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var p struct{ Cmd string }
							ssh.Unmarshal(req.Payload, &p)
							req.Reply(true, nil)
							cmd := exec.Command("sh", "-c", p.Cmd)
							cmd.Env = append(os.Environ(), "HOME="+home)
							cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
							status := 0
							if err := cmd.Run(); err != nil {
								status = 1
							}
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ S uint32 }{uint32(status)}))
							return
						}
					}()
				}
			}()
		}
	}()
	c, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "renter", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCheckMachineOverSSH(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	c := shellSSHServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := checkMachine(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if r.Cores < 1 || r.CPUSeconds <= 0 || r.DiskWriteMBps <= 0 || r.TunnelMbps <= 0 || r.Client == "" {
		t.Fatalf("incomplete report: %+v", r)
	}
	t.Logf("%+v", r)
}

func buildGuest(t *testing.T) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "nvto-guest")
	cmd := exec.Command("go", "build", "-o", out, "../nvto-guest")
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building nvto-guest: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckWithGuest(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	old := guestBinary
	defer func() { guestBinary = old }()
	guestBinary = buildGuest(t)
	c := shellSSHServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, err := checkMachine(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "guest" || !r.Verified || len(r.Failures) > 0 || r.CPU1MBps <= 0 || r.CPUNMBps <= r.CPU1MBps ||
		r.MemoryMBps <= 0 || r.MemoryTestedBytes < 1<<30 || r.DiskWriteMBps <= 0 || r.TunnelMbps <= 0 {
		t.Fatalf("report: %+v", r)
	}
	t.Logf("%+v", r)
}

// A measuring program that claims a machine and waits instead of working
// is found out: its answers are wrong.
func TestCheckCatchesAFakeGuest(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	old := guestBinary
	defer func() { guestBinary = old }()
	fake := `#!/bin/sh
case "$1" in
info) echo '{"model":"Mac16,11","cpu":"Apple M4 Pro","cores":12,"memory_bytes":68719476736,"disk_bytes":2000000000000,"disk_free_bytes":1900000000000}' ;;
*) sleep 0.2; h=$(printf '%064d' 0); echo "{\"answers\":[[\"$h\"]]}" ;;
esac
`
	guestBinary = []byte(fake)
	c := shellSSHServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := checkMachine(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verified || len(r.Failures) != 4 || r.CPU1MBps != 0 || r.MemoryMBps != 0 {
		t.Fatalf("fake guest not caught: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Failures, ","), "wrong answers") {
		t.Fatalf("failures: %v", r.Failures)
	}
}
