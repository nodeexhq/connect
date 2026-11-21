// Package pretest is the client side of a node's connection test: what
// Connect runs, before an order, against the gate's "test" service of the
// machine a renter has reserved, to measure the connection a remote desktop
// would get.
//
// After the gate has accepted the connection (NODEEX1 test <token>), the two
// sides exchange frames: a 1-byte type and a fixed body, every integer
// unsigned and big-endian. The client leads, through four phases in order:
//
//	P  ping      C: PING seq u32, t u64        S: PONG seq, t (echoed)
//	D  download  C: DOWN bytes u32             S: DATA len u16, payload ... DONE sent u64, elapsed_us u64
//	U  upload    C: UP bytes u32, DATA ..., END sent u64
//	                                           S: UPRESULT received u64, counted u64, elapsed_us u64
//	B  bye       C: BYE                        S: BYE
//
// Either side may send ERR code u8, n u8, reason[n] at any point; the test
// has then failed for that reason. The Miner has its own copy of this
// protocol; the two must agree byte for byte.
package pretest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame types.
const (
	TypePing     byte = 0x01
	TypePong     byte = 0x02
	TypeDown     byte = 0x10
	TypeData     byte = 0x11
	TypeDone     byte = 0x12
	TypeUp       byte = 0x20
	TypeEnd      byte = 0x21
	TypeUpResult byte = 0x22
	TypeBye      byte = 0x30
	TypeErr      byte = 0x7F
)

// MaxDataLen is the largest DATA payload the client sends.
const MaxDataLen = 16384

// ErrProtocol is a frame that does not belong where it came.
var ErrProtocol = errors.New("pretest: unexpected data from the machine")

// Frame is one decoded frame. Which fields are set depends on Type. A DATA
// frame's payload is not kept: only its length.
type Frame struct {
	Type byte
	// PING and PONG.
	Seq uint32
	T   uint64
	// DOWN and UP.
	Bytes uint32
	// DATA: the payload's length.
	Len int
	// DONE (Sent, Elapsed), END (Sent), UPRESULT (Received, Counted, Elapsed).
	Sent, Received, Counted uint64
	Elapsed                 uint64 // microseconds
	// ERR.
	Code   byte
	Reason string
}

// ReadFrame reads one frame of any type.
func ReadFrame(r *bufio.Reader) (Frame, error) {
	t, err := r.ReadByte()
	if err != nil {
		return Frame{}, err
	}
	f := Frame{Type: t}
	var body [24]byte
	read := func(n int) ([]byte, error) {
		b := body[:n]
		if _, err := io.ReadFull(r, b); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return b, nil
	}
	be := binary.BigEndian
	switch t {
	case TypePing, TypePong:
		b, err := read(12)
		if err != nil {
			return f, err
		}
		f.Seq, f.T = be.Uint32(b), be.Uint64(b[4:])
	case TypeDown, TypeUp:
		b, err := read(4)
		if err != nil {
			return f, err
		}
		f.Bytes = be.Uint32(b)
	case TypeData:
		b, err := read(2)
		if err != nil {
			return f, err
		}
		f.Len = int(be.Uint16(b))
		if _, err := r.Discard(f.Len); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return f, err
		}
	case TypeDone:
		b, err := read(16)
		if err != nil {
			return f, err
		}
		f.Sent, f.Elapsed = be.Uint64(b), be.Uint64(b[8:])
	case TypeEnd:
		b, err := read(8)
		if err != nil {
			return f, err
		}
		f.Sent = be.Uint64(b)
	case TypeUpResult:
		b, err := read(24)
		if err != nil {
			return f, err
		}
		f.Received, f.Counted, f.Elapsed = be.Uint64(b), be.Uint64(b[8:]), be.Uint64(b[16:])
	case TypeBye:
	case TypeErr:
		b, err := read(2)
		if err != nil {
			return f, err
		}
		f.Code = b[0]
		reason := make([]byte, b[1])
		if _, err := io.ReadFull(r, reason); err != nil {
			return f, io.ErrUnexpectedEOF
		}
		f.Reason = string(reason)
	default:
		return f, fmt.Errorf("%w: frame type 0x%02x", ErrProtocol, t)
	}
	return f, nil
}

// AppendFrame appends the encoding of f to b. A DATA frame's payload is the
// given one; f.Len is ignored.
func AppendFrame(b []byte, f Frame, payload []byte) []byte {
	be := binary.BigEndian
	b = append(b, f.Type)
	switch f.Type {
	case TypePing, TypePong:
		b = be.AppendUint64(be.AppendUint32(b, f.Seq), f.T)
	case TypeDown, TypeUp:
		b = be.AppendUint32(b, f.Bytes)
	case TypeData:
		b = append(be.AppendUint16(b, uint16(len(payload))), payload...)
	case TypeDone:
		b = be.AppendUint64(be.AppendUint64(b, f.Sent), f.Elapsed)
	case TypeEnd:
		b = be.AppendUint64(b, f.Sent)
	case TypeUpResult:
		b = be.AppendUint64(be.AppendUint64(be.AppendUint64(b, f.Received), f.Counted), f.Elapsed)
	case TypeErr:
		reason := f.Reason
		if len(reason) > 255 {
			reason = reason[:255]
		}
		b = append(append(b, f.Code, byte(len(reason))), reason...)
	}
	return b
}
