package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/nvtohq/connect/internal/workproof"
	"golang.org/x/crypto/ssh"
)

// The measuring program's place in the machine, removed after the check.
const (
	guestDir  = `"$HOME/.nvto-check"`
	guestPath = guestDir + `/g`
)

// The checked jobs. Their size is fixed so results compare across machines.
const (
	cpuChunk     = 16 << 20
	cpu1Chunks   = 64 // 1 GiB on one core
	cpuNChunks   = 32 // 512 MiB on each core
	memChunk     = 4 << 20
	memShare     = 0.6 // of the memory the machine reports
	memMax       = 24 << 30
	diskChunk    = 16 << 20
	diskChunks   = 64 // 1 GiB
	samplesEach  = 6
	guestTimeout = 3 * time.Minute
)

// runGuest runs the measuring program and decodes its JSON answer, timing it.
func runGuest(ctx context.Context, c *ssh.Client, args string, out any) (time.Duration, error) {
	s, err := c.NewSession()
	if err != nil {
		return 0, err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, guestTimeout)
	defer cancel()
	go func() { <-ctx.Done(); s.Close() }()
	var stdout bytes.Buffer
	s.Stdout = &stdout
	start := time.Now()
	err = s.Run(guestPath + " " + args)
	elapsed := time.Since(start)
	if err != nil {
		return elapsed, err
	}
	return elapsed, json.Unmarshal(stdout.Bytes(), out)
}

func newKey() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

func randInt(n int) int {
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(v.Int64())
}

// verify checks a few answers picked at random against what this computer
// works out itself.
func verify(key []byte, answers [][]string, workers, chunks, size int) bool {
	if len(answers) != workers {
		return false
	}
	for _, a := range answers {
		if len(a) != chunks {
			return false
		}
	}
	scratch := make([]byte, size)
	for i := 0; i < samplesEach; i++ {
		w, ch := randInt(workers), randInt(chunks)
		want := workproof.ChunkAnswer(workproof.WorkerKey(key, w), ch, size, scratch)
		if answers[w][ch] != hex.EncodeToString(want[:]) {
			return false
		}
	}
	return true
}

func mbpsOf(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return round2(float64(bytes) / 1e6 / d.Seconds())
}

// checkWithGuest copies the measuring program into the machine and has it
// describe the machine and do the checked jobs.
func checkWithGuest(ctx context.Context, c *ssh.Client) (machineReport, error) {
	var r machineReport
	s, err := c.NewSession()
	if err != nil {
		return r, err
	}
	s.Stdin = bytes.NewReader(guestBinary)
	err = s.Run(`umask 077 && mkdir -p ` + guestDir + ` && cat > ` + guestPath + `.tmp && chmod 700 ` + guestPath + `.tmp && mv -f ` + guestPath + `.tmp ` + guestPath)
	s.Close()
	if err != nil {
		return r, fmt.Errorf("copying the measuring program: %w", err)
	}
	defer func() {
		if s, err := c.NewSession(); err == nil {
			s.Run(`rm -rf ` + guestDir)
			s.Close()
		}
	}()

	var info struct {
		Model         string `json:"model"`
		CPU           string `json:"cpu"`
		Cores         int    `json:"cores"`
		MemoryBytes   int64  `json:"memory_bytes"`
		DiskBytes     int64  `json:"disk_bytes"`
		DiskFreeBytes int64  `json:"disk_free_bytes"`
	}
	if _, err := runGuest(ctx, c, "info", &info); err != nil {
		return r, fmt.Errorf("reading system information: %w", err)
	}
	if info.Cores < 1 || info.MemoryBytes < 1<<30 {
		return r, errors.New("unexpected system information")
	}
	r = machineReport{Model: info.Model, CPU: info.CPU, Cores: info.Cores, MemoryBytes: info.MemoryBytes,
		DiskBytes: info.DiskBytes, DiskFreeBytes: min(info.DiskFreeBytes, info.DiskBytes),
		Client: "nvto-connect " + version, Method: "guest"}

	type answers struct {
		Answers json.RawMessage `json:"answers"` // [worker][chunk] hex SHA-256
	}
	job := func(name, args string, workers, chunks, size int, key []byte) (time.Duration, bool) {
		var a answers
		d, err := runGuest(ctx, c, args, &a)
		if err != nil {
			r.Failures = append(r.Failures, name+" test did not finish")
			return 0, false
		}
		var grid [][]string
		json.Unmarshal(a.Answers, &grid)
		if !verify(key, grid, workers, chunks, size) {
			r.Failures = append(r.Failures, name+" test gave wrong answers")
			return 0, false
		}
		return d, true
	}

	k := newKey()
	if d, ok := job("cpu", fmt.Sprintf("cpu %x 1 %d %d", k, cpu1Chunks, cpuChunk), 1, cpu1Chunks, cpuChunk, k); ok {
		r.CPU1MBps = mbpsOf(cpu1Chunks*cpuChunk, d)
	}
	k = newKey()
	if d, ok := job("all-core", fmt.Sprintf("cpu %x %d %d %d", k, info.Cores, cpuNChunks, cpuChunk), info.Cores, cpuNChunks, cpuChunk, k); ok {
		r.CPUNMBps = mbpsOf(int64(info.Cores)*cpuNChunks*cpuChunk, d)
	}
	memChunks := int(min(float64(info.MemoryBytes)*memShare, memMax) / memChunk)
	k = newKey()
	if d, ok := job("memory", fmt.Sprintf("mem %x %d %d", k, memChunks, memChunk), 1, memChunks, memChunk, k); ok {
		r.MemoryTestedBytes = int64(memChunks) * memChunk
		r.MemoryMBps = mbpsOf(r.MemoryTestedBytes, d)
	}
	k = newKey()
	if d, ok := job("disk", fmt.Sprintf("disk %x %d %d", k, diskChunks, diskChunk), 1, diskChunks, diskChunk, k); ok {
		r.DiskWriteMBps = mbpsOf(diskChunks*diskChunk, d)
	}
	if mbps, err := tunnelSpeed(ctx, c); err == nil {
		r.TunnelMbps = mbps
	}
	r.Verified = len(r.Failures) == 0
	return r, nil
}
