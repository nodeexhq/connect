// Package workproof is the arithmetic of the machine check: work the
// rented machine must really do, whose answers this computer can check
// cheaply.
//
// Data is the AES-256-CTR keystream of a key chosen by the renter's
// computer for each test. A chunk is a run of that keystream at a known
// offset, and its answer is the SHA-256 of the chunk. Because CTR can start
// anywhere, this computer can work out any one chunk's answer on its own,
// so it checks a few chunks picked at random and need not redo all of the
// machine's work. Without the key the machine cannot know the answers in
// advance, and a program that only waits instead of working gives answers
// that do not match.
package workproof

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"io"
)

// WorkerKey derives a separate key for each parallel worker.
func WorkerKey(key []byte, worker int) []byte {
	h := sha256.New()
	h.Write(key)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(worker))
	h.Write([]byte("worker"))
	h.Write(b[:])
	return h.Sum(nil)
}

// Stream returns the keystream for key starting at byte offset, which must
// be a multiple of 16.
func Stream(key []byte, offset int64) cipher.Stream {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // keys are always 32 bytes here
	}
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(offset/aes.BlockSize))
	return cipher.NewCTR(block, iv)
}

// Fill writes the keystream at offset into buf.
func Fill(key []byte, offset int64, buf []byte) {
	clear(buf)
	Stream(key, offset).XORKeyStream(buf, buf)
}

// ChunkAnswer is the answer for chunk index of size bytes: the SHA-256 of
// the keystream there. scratch, if large enough, is used as a buffer.
func ChunkAnswer(key []byte, index int, size int, scratch []byte) [32]byte {
	if len(scratch) < size {
		scratch = make([]byte, size)
	}
	buf := scratch[:size]
	Fill(key, int64(index)*int64(size), buf)
	return sha256.Sum256(buf)
}

// HashReader returns the SHA-256 of r's contents.
func HashReader(r io.Reader) ([32]byte, error) {
	h := sha256.New()
	_, err := io.Copy(h, r)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, err
}
