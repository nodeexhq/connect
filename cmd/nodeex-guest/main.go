//go:build darwin

// nodeex-guest is the small program NodeEx Connect copies into a rented
// machine to check it. It runs only Go and the system calls below, not the
// machine's own command-line tools, so replacing those tools changes
// nothing. Every timed job it does is checked by Connect against answers it
// can work out on its own (package workproof).
//
//	nodeex-guest info
//	nodeex-guest cpu KEY WORKERS CHUNKS CHUNK_BYTES
//	nodeex-guest mem KEY CHUNKS CHUNK_BYTES
//	nodeex-guest disk KEY CHUNKS CHUNK_BYTES
//
// Each writes one JSON object on standard output.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"sync"

	"github.com/nodeexhq/connect/internal/workproof"
	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: nodeex-guest info|cpu|mem|disk ...")
	}
	var out any
	switch os.Args[1] {
	case "info":
		out = info()
	case "cpu":
		key, n := args(5)
		out = cpu(key, n[0], n[1], n[2])
	case "mem":
		key, n := args(4)
		out = mem(key, n[0], n[1])
	case "disk":
		key, n := args(4)
		out = disk(key, n[0], n[1])
	default:
		fail("unknown command")
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "nodeex-guest:", msg)
	os.Exit(2)
}

// args reads a hex key and the numbers after it.
func args(want int) ([]byte, []int) {
	if len(os.Args) != want+1 {
		fail("wrong number of arguments")
	}
	key, err := hex.DecodeString(os.Args[2])
	if err != nil || len(key) != 32 {
		fail("bad key")
	}
	var n []int
	for _, a := range os.Args[3:] {
		v, err := strconv.Atoi(a)
		if err != nil || v < 1 || v > 1<<30 {
			fail("bad number")
		}
		n = append(n, v)
	}
	return key, n
}

func info() map[string]any {
	model, _ := unix.Sysctl("hw.model")
	cpu, _ := unix.Sysctl("machdep.cpu.brand_string")
	cores, _ := unix.SysctlUint32("hw.ncpu")
	memory, _ := unix.SysctlUint64("hw.memsize")
	var fs unix.Statfs_t
	if unix.Statfs("/System/Volumes/Data", &fs) != nil {
		unix.Statfs("/", &fs)
	}
	return map[string]any{
		"model": model, "cpu": cpu, "cores": cores, "memory_bytes": memory,
		"disk_bytes": fs.Blocks * uint64(fs.Bsize), "disk_free_bytes": fs.Bavail * uint64(fs.Bsize),
	}
}

func hexes(h [][32]byte) []string {
	out := make([]string, len(h))
	for i := range h {
		out[i] = hex.EncodeToString(h[i][:])
	}
	return out
}

// cpu has each worker, in parallel, hash its own run of keystream.
func cpu(key []byte, workers, chunks, size int) map[string]any {
	answers := make([][]string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := workproof.WorkerKey(key, w)
			buf := make([]byte, size)
			h := make([][32]byte, chunks)
			for c := 0; c < chunks; c++ {
				h[c] = workproof.ChunkAnswer(k, c, size, buf)
			}
			answers[w] = hexes(h)
		}()
	}
	wg.Wait()
	return map[string]any{"answers": answers}
}

// mem fills that much memory with keystream, then reads it back in an
// order taken from the key and hashes each chunk. A machine that does not
// have the memory it claims has to page it out and in again, which shows
// in the time Connect measures.
func mem(key []byte, chunks, size int) map[string]any {
	k := workproof.WorkerKey(key, 0)
	buf := make([]byte, chunks*size)
	var seed [32]byte
	copy(seed[:], k)
	order := rand.New(rand.NewChaCha8(seed)).Perm(chunks)
	h := make([][32]byte, chunks)
	// On every core, so that memory, not one core, sets the pace.
	workers := max(1, runtime.NumCPU())
	each := func(job func(i int)) {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := w; i < chunks; i += workers {
					job(i)
				}
			}()
		}
		wg.Wait()
	}
	each(func(c int) { workproof.Fill(k, int64(c)*int64(size), buf[c*size:(c+1)*size]) })
	each(func(i int) {
		c := order[i]
		h[c] = sha256.Sum256(buf[c*size : (c+1)*size])
	})
	return map[string]any{"answers": [][]string{hexes(h)}}
}

// disk writes keystream to a file, flushes it to the disk, then reads it
// back and hashes each chunk.
func disk(key []byte, chunks, size int) map[string]any {
	k := workproof.WorkerKey(key, 0)
	dir := os.TempDir()
	if home, err := os.UserHomeDir(); err == nil {
		dir = home
	}
	f, err := os.CreateTemp(dir, ".nodeex-check-*")
	if err != nil {
		fail(err.Error())
	}
	defer os.Remove(f.Name())
	defer f.Close()
	buf := make([]byte, size)
	for c := 0; c < chunks; c++ {
		workproof.Fill(k, int64(c)*int64(size), buf)
		if _, err := f.Write(buf); err != nil {
			fail(err.Error())
		}
	}
	// F_FULLFSYNC: on macOS a plain fsync may leave data in the drive's cache.
	if _, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		f.Sync()
	}
	h := make([][32]byte, chunks)
	for c := 0; c < chunks; c++ {
		if _, err := f.ReadAt(buf, int64(c)*int64(size)); err != nil {
			fail(err.Error())
		}
		h[c] = sha256.Sum256(buf)
	}
	return map[string]any{"answers": [][]string{hexes(h)}}
}
