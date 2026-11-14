package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os/exec"
	"runtime"
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
							cmd.Stdout, cmd.Stderr = ch, ch.Stderr()
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
