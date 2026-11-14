package workproof

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestChunksAreSeekable(t *testing.T) {
	key := WorkerKey(bytes.Repeat([]byte{7}, 32), 3)
	const size = 1 << 16
	all := make([]byte, 4*size)
	Fill(key, 0, all)
	for i := 0; i < 4; i++ {
		if ChunkAnswer(key, i, size, nil) != sha256.Sum256(all[i*size:(i+1)*size]) {
			t.Fatalf("chunk %d does not match the stream from the start", i)
		}
	}
	if bytes.Equal(WorkerKey(key, 0), WorkerKey(key, 1)) {
		t.Fatal("workers share a key")
	}
}
