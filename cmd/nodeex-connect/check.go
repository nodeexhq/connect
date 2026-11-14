package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Checking the machine. The owner controls the node and its Miner, and the
// Miner could claim a better machine than it is; this program is the
// renter's own. When it connects it looks at the machine from the inside,
// times a few standard jobs, and reports to the platform, which compares
// the report with what the node claims.

// machineReport is what the check found. The field names are the
// platform's (POST /v1/leases/{id}/check).
type machineReport struct {
	Model         string  `json:"model"`
	CPU           string  `json:"cpu"`
	Cores         int     `json:"cores"`
	MemoryBytes   int64   `json:"memory_bytes"`
	DiskBytes     int64   `json:"disk_bytes"`
	DiskFreeBytes int64   `json:"disk_free_bytes"`
	CPUSeconds    float64 `json:"cpu_seconds,omitempty"`
	DiskWriteMBps float64 `json:"disk_write_mbps,omitempty"`
	TunnelMbps    float64 `json:"tunnel_mbps,omitempty"`
	Client        string  `json:"client"`
	// How the check was made: "guest" with Connect's own measuring
	// program, whose answers were checked, or "tools" with the machine's.
	Method   string   `json:"method"`
	Verified bool     `json:"verified"`
	Failures []string `json:"failures,omitempty"`
	// Throughput of the checked jobs, in MB/s: one core, all cores, and
	// memory (filling and reading back MemoryTestedBytes).
	CPU1MBps          float64 `json:"cpu1_mbps,omitempty"`
	CPUNMBps          float64 `json:"cpun_mbps,omitempty"`
	MemoryTestedBytes int64   `json:"memory_tested_bytes,omitempty"`
	MemoryMBps        float64 `json:"memory_mbps,omitempty"`
}

// checkEvery is how often one computer checks the same lease's machine.
const checkEvery = 24 * time.Hour

const (
	// The machine's description, from macOS itself.
	checkInfo = `sysctl -n hw.model machdep.cpu.brand_string hw.ncpu hw.memsize && (df -k /System/Volumes/Data 2>/dev/null || df -k /) | tail -1`
	// 4 GiB through SHA-256 on one core with the system's own LibreSSL:
	// about two seconds on a recent Apple chip.
	checkCPU = `dd if=/dev/zero bs=1048576 count=4096 2>/dev/null | /usr/bin/openssl dgst -sha256 >/dev/null`
	// 1 GiB written to the renter's disk and flushed, then removed.
	checkDisk = `f=$(mktemp "${TMPDIR:-/tmp}/nodeex-check.XXXXXX") || exit 1; dd if=/dev/zero of="$f" bs=1048576 count=1024 2>/dev/null && sync; rc=$?; rm -f "$f"; exit $rc`
	// 32 MiB from the machine to this computer, through the same tunnel
	// SSH and remote desktop use.
	checkTunnelBytes = 32 << 20
)

// parseInfo reads checkInfo's output: four sysctl lines and one df line.
func parseInfo(out string) (machineReport, error) {
	var r machineReport
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 5 {
		return r, errors.New("unexpected system information")
	}
	r.Model = strings.TrimSpace(lines[0])
	r.CPU = strings.TrimSpace(lines[1])
	var err error
	if r.Cores, err = strconv.Atoi(strings.TrimSpace(lines[2])); err != nil {
		return r, fmt.Errorf("cpu count: %w", err)
	}
	if r.MemoryBytes, err = strconv.ParseInt(strings.TrimSpace(lines[3]), 10, 64); err != nil {
		return r, fmt.Errorf("memory: %w", err)
	}
	// Filesystem 1024-blocks Used Available Capacity ...
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return r, errors.New("unexpected disk information")
	}
	total, err1 := strconv.ParseInt(f[1], 10, 64)
	free, err2 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil {
		return r, errors.New("unexpected disk information")
	}
	r.DiskBytes, r.DiskFreeBytes = total*1024, min(free, total)*1024
	return r, nil
}

// runTimed runs a command on the machine and returns how long it took.
func runTimed(ctx context.Context, c *ssh.Client, cmd string, limit time.Duration) (time.Duration, error) {
	s, err := c.NewSession()
	if err != nil {
		return 0, err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	go func() { <-ctx.Done(); s.Close() }()
	start := time.Now()
	err = s.Run(cmd)
	return time.Since(start), err
}

// checkMachine looks at the machine over SSH, with Connect's own measuring
// program when this build carries one.
func checkMachine(ctx context.Context, c *ssh.Client) (machineReport, error) {
	if len(guestBinary) > 0 {
		return checkWithGuest(ctx, c)
	}
	return checkWithTools(ctx, c)
}

// checkWithTools uses the machine's own commands. Only the description is
// required; a timing that fails is left out of the report.
func checkWithTools(ctx context.Context, c *ssh.Client) (machineReport, error) {
	s, err := c.NewSession()
	if err != nil {
		return machineReport{}, err
	}
	var out bytes.Buffer
	s.Stdout = &out
	err = s.Run(checkInfo)
	s.Close()
	if err != nil {
		return machineReport{}, fmt.Errorf("reading system information: %w", err)
	}
	r, err := parseInfo(out.String())
	if err != nil {
		return r, err
	}
	r.Client = "nodeex-connect " + version
	r.Method = "tools"
	if d, err := runTimed(ctx, c, checkCPU, 60*time.Second); err == nil {
		r.CPUSeconds = round2(d.Seconds())
	}
	if d, err := runTimed(ctx, c, checkDisk, 60*time.Second); err == nil && d > 0 {
		r.DiskWriteMBps = round2(1024 * 1.048576 / d.Seconds())
	}
	if mbps, err := tunnelSpeed(ctx, c); err == nil {
		r.TunnelMbps = mbps
	}
	return r, nil
}

// tunnelSpeed times checkTunnelBytes sent from the machine to here.
func tunnelSpeed(ctx context.Context, c *ssh.Client) (float64, error) {
	s, err := c.NewSession()
	if err != nil {
		return 0, err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	go func() { <-ctx.Done(); s.Close() }()
	pipe, err := s.StdoutPipe()
	if err != nil {
		return 0, err
	}
	start := time.Now()
	if err := s.Start(fmt.Sprintf("head -c %d /dev/zero", checkTunnelBytes)); err != nil {
		return 0, err
	}
	n, _ := io.Copy(io.Discard, pipe)
	elapsed := time.Since(start)
	s.Wait()
	if n < 1<<20 {
		return 0, errors.New("too little data")
	}
	return round2(float64(n) * 8 / 1e6 / elapsed.Seconds()), nil
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// checkLease checks the lease's machine if this computer has not done so
// recently, reports to the platform, and returns its verdict.
func checkLease(ctx context.Context, s session, dir, leaseID string, port int) {
	a := loadAccess(dir, leaseID)
	if time.Since(a.CheckedAt) < checkEvery {
		return
	}
	c, err := machineLogin(ctx, dir, port)
	if err != nil {
		return
	}
	defer c.Close()
	log.Printf("  checking the machine (about 15 seconds; it is busy meanwhile)")
	rep, err := checkMachine(ctx, c)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("  machine check: %v", err)
		}
		return
	}
	var res struct {
		Verdict  string   `json:"verdict"`
		Problems []string `json:"problems"`
	}
	if err := call(ctx, s, "POST", "/v1/leases/"+leaseID+"/check", rep, &res); err != nil {
		var ae *apiError
		if !(errors.As(err, &ae) && ae.Code == "check_too_soon") {
			log.Printf("  machine check could not be reported: %v", err)
		}
		return
	}
	a = loadAccess(dir, leaseID)
	a.CheckedAt = time.Now()
	saveAccess(dir, leaseID, a)
	emit("check", map[string]any{"verdict": res.Verdict, "problems": res.Problems,
		"cpu": rep.CPU, "cores": rep.Cores, "memory_bytes": rep.MemoryBytes, "disk_bytes": rep.DiskBytes, "tunnel_mbps": rep.TunnelMbps})
	if res.Verdict == "ok" {
		log.Printf("  machine check: matches what was ordered (%s, %d cores, %.0f GB memory, %.0f GB disk)",
			rep.CPU, rep.Cores, float64(rep.MemoryBytes)/(1<<30), float64(rep.DiskBytes)/1e9)
		return
	}
	log.Printf("  machine check: this machine does not match what was ordered:")
	for _, p := range res.Problems {
		log.Printf("    - %s", p)
	}
	log.Printf("  NodeEx has been told. If it is not what you ordered, write to hello@nodeex.com about a refund.")
}
