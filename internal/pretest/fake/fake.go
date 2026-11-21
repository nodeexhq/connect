// Package fake is a stand-in for the Miner's side of the connection test,
// for tests: it answers the client as the protocol says, optionally slowly,
// lossily or with an error.
package fake

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/nodeexhq/connect/internal/pretest"
)

// Options shape the fake machine's behaviour. The zero value is a fast,
// faultless machine.
type Options struct {
	// PongDelay is added before each PONG.
	PongDelay time.Duration
	// Drop, if set, says which PINGs go unanswered.
	Drop func(seq uint32) bool
	// LatePongs answers dropped PINGs anyway, this long after they came.
	LatePongs time.Duration
	// DownRate and UpRate cap the bytes per second sent and read; 0 is no
	// cap.
	DownRate, UpRate int64
	// MaxDown stops the download after this long; 0 is no limit.
	MaxDown time.Duration
	// FailOn sends ERR FailCode FailReason when a frame of this type comes.
	FailOn     byte
	FailCode   byte
	FailReason string
}

const chunk = 16384

// Serve answers one client on conn until BYE, an error or an ERR it sends.
func Serve(conn net.Conn, o Options) error {
	br := bufio.NewReader(conn)
	w := &lockedWriter{w: conn}
	var upWant, upGot, upFirst uint64
	var upFrames int
	var upStart time.Time
	payload := make([]byte, chunk)
	for {
		f, err := pretest.ReadFrame(br)
		if err != nil {
			return err
		}
		if o.FailOn != 0 && f.Type == o.FailOn {
			w.write(pretest.Frame{Type: pretest.TypeErr, Code: o.FailCode, Reason: o.FailReason}, nil)
			return nil
		}
		switch f.Type {
		case pretest.TypePing:
			pong := pretest.Frame{Type: pretest.TypePong, Seq: f.Seq, T: f.T}
			if o.Drop != nil && o.Drop(f.Seq) {
				if o.LatePongs > 0 {
					time.AfterFunc(o.LatePongs, func() { w.write(pong, nil) })
				}
				continue
			}
			time.Sleep(o.PongDelay)
			if err := w.write(pong, nil); err != nil {
				return err
			}
		case pretest.TypeDown:
			start := time.Now()
			var sent uint64
			for sent < uint64(f.Bytes) {
				if o.MaxDown > 0 && time.Since(start) > o.MaxDown {
					break
				}
				n := min(uint64(chunk), uint64(f.Bytes)-sent)
				if err := w.write(pretest.Frame{Type: pretest.TypeData}, payload[:n]); err != nil {
					return err
				}
				sent += n
				pace(start, sent, o.DownRate)
			}
			if err := w.write(pretest.Frame{Type: pretest.TypeDone, Sent: sent, Elapsed: uint64(time.Since(start).Microseconds())}, nil); err != nil {
				return err
			}
		case pretest.TypeUp:
			upWant, upGot, upFirst, upFrames = uint64(f.Bytes), 0, 0, 0
		case pretest.TypeData:
			if upFrames == 0 {
				upStart, upFirst = time.Now(), uint64(f.Len)
			}
			upFrames++
			upGot += uint64(f.Len)
			if upGot > upWant {
				w.write(pretest.Frame{Type: pretest.TypeErr, Code: 2, Reason: "too-much-data"}, nil)
				return fmt.Errorf("fake: client sent %d of %d bytes", upGot, upWant)
			}
			pace(upStart, upGot-upFirst, o.UpRate)
		case pretest.TypeEnd:
			var elapsed uint64
			if upFrames > 0 {
				elapsed = uint64(time.Since(upStart).Microseconds())
			}
			if err := w.write(pretest.Frame{Type: pretest.TypeUpResult, Received: upGot, Counted: upGot - upFirst, Elapsed: elapsed}, nil); err != nil {
				return err
			}
		case pretest.TypeBye:
			return w.write(pretest.Frame{Type: pretest.TypeBye}, nil)
		default:
			w.write(pretest.Frame{Type: pretest.TypeErr, Code: 1, Reason: "protocol"}, nil)
			return fmt.Errorf("fake: unexpected frame 0x%02x", f.Type)
		}
	}
}

// pace sleeps until n bytes since start are within rate.
func pace(start time.Time, n uint64, rate int64) {
	if rate <= 0 {
		return
	}
	due := start.Add(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
	if d := time.Until(due); d > 0 {
		time.Sleep(d)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) write(f pretest.Frame, payload []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.w.Write(pretest.AppendFrame(nil, f, payload))
	return err
}
