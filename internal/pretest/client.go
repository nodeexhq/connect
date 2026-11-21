package pretest

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"slices"
	"time"
)

// The client's timing, from the protocol.
var (
	pingInterval = 50 * time.Millisecond   // after a PONG, before the next PING
	pingTimeout  = time.Second             // a PING not answered by then is lost
	pingWindow   = 2500 * time.Millisecond // the ping phase stops after this
	upDuration   = 3 * time.Second         // the upload stops sending after this
)

// MinPings is the fewest answered pings a test can be rated on.
const MinPings = 3

// Limits are a test ticket's caps, as the platform gives them. The client
// never asks for more.
type Limits struct {
	Pings        int   `json:"pings"`
	MaxDownBytes int64 `json:"max_down_bytes"`
	MaxUpBytes   int64 `json:"max_up_bytes"`
	MaxSeconds   int   `json:"max_seconds"`
}

// Result is what one test measured.
type Result struct {
	LatencyMS float64 // median round trip of the answered pings
	JitterMS  float64 // mean change in round trip between consecutive answered pings
	DownMbps  float64 // machine to this computer
	UpMbps    float64 // this computer to the machine

	PingsSent, PingsAnswered int
	DownBytes, UpBytes       int64
}

// RemoteError is an ERR frame: the machine stopped the test.
type RemoteError struct {
	Code   byte
	Reason string
}

func (e *RemoteError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("the machine stopped the test (code %d)", e.Code)
	}
	return "the machine stopped the test: " + e.Reason
}

var (
	// ErrTooMuchLoss: more than half the pings were lost, or fewer than
	// MinPings were answered in the ping window.
	ErrTooMuchLoss = errors.New("too few of the test's pings were answered: the connection to this machine is losing too much")
	// ErrTimeout: the test did not finish within the ticket's time.
	ErrTimeout = errors.New("the test did not finish in the time allowed: the connection to this machine is too slow")
	// ErrClosed: the machine closed the connection before the test ended.
	ErrClosed = errors.New("the machine closed the connection before the test finished")
)

var errWait = errors.New("pretest: waited long enough")

type received struct {
	f   Frame
	at  time.Time
	err error
}

type client struct {
	ctx    context.Context
	conn   net.Conn
	frames chan received
	done   chan struct{}
	epoch  time.Time
	buf    []byte
}

// Run runs the whole test on conn, which the gate has just accepted for the
// test service, and returns what it measured. It sets conn's deadline to the
// ticket's time limit; the caller closes conn.
func Run(ctx context.Context, conn net.Conn, lim Limits) (Result, error) {
	if lim.Pings < 1 || lim.MaxSeconds < 1 || lim.MaxDownBytes < 0 || lim.MaxUpBytes < 0 {
		return Result{}, fmt.Errorf("the test ticket's limits are unusable: %+v", lim)
	}
	deadline := time.Now().Add(time.Duration(lim.MaxSeconds) * time.Second)
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	c := &client{ctx: ctx, conn: conn, frames: make(chan received, 64), done: make(chan struct{}), epoch: time.Now()}
	defer close(c.done)
	go c.readLoop()

	var r Result
	err := c.ping(lim.Pings, &r)
	if err == nil {
		err = c.download(clampU32(lim.MaxDownBytes), &r)
	}
	if err == nil {
		err = c.upload(clampU32(lim.MaxUpBytes), &r)
	}
	if err == nil {
		err = c.bye()
	}
	return r, err
}

func clampU32(n int64) uint32 {
	return uint32(min(max(n, 0), math.MaxUint32))
}

func (c *client) readLoop() {
	br := bufio.NewReaderSize(c.conn, 64<<10)
	for {
		f, err := ReadFrame(br)
		select {
		case c.frames <- received{f, time.Now(), err}:
		case <-c.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// fail turns a connection error into what the renter should hear.
func (c *client) fail(err error) error {
	switch {
	case c.ctx.Err() != nil:
		return c.ctx.Err()
	case errors.Is(err, os.ErrDeadlineExceeded):
		return ErrTimeout
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		return ErrClosed
	case errors.Is(err, ErrProtocol):
		return err
	}
	return fmt.Errorf("the test connection failed: %w", err)
}

func (c *client) send(f Frame, payload []byte) error {
	c.buf = AppendFrame(c.buf[:0], f, payload)
	if _, err := c.conn.Write(c.buf); err != nil {
		return c.fail(err)
	}
	return nil
}

// recv returns the next frame, or errWait if none came before until (zero:
// wait for as long as the connection's deadline allows). An ERR frame is
// returned as a *RemoteError.
func (c *client) recv(until time.Time) (Frame, time.Time, error) {
	var timeout <-chan time.Time
	if !until.IsZero() {
		t := time.NewTimer(time.Until(until))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case r := <-c.frames:
		if r.err != nil {
			return r.f, r.at, c.fail(r.err)
		}
		if r.f.Type == TypeErr {
			return r.f, r.at, &RemoteError{r.f.Code, r.f.Reason}
		}
		return r.f, r.at, nil
	case <-timeout:
		return Frame{}, time.Time{}, errWait
	}
}

func unexpected(phase string, f Frame) error {
	return fmt.Errorf("%w (frame 0x%02x during the %s)", ErrProtocol, f.Type, phase)
}

// mono is this side's monotonic clock, in nanoseconds since the test began.
func (c *client) mono(t time.Time) uint64 { return uint64(t.Sub(c.epoch)) }

// ping sends at most n PINGs, one at a time, the next pingInterval after a
// PONG or straight after a ping is lost, for at most pingWindow. It gives up
// as soon as more than half of n are lost, since the test has failed by
// then. A ping still unanswered when the window closes is not counted.
func (c *client) ping(n int, r *Result) error {
	var rtts []float64
	lost := 0
	end := time.Now().Add(pingWindow)
	for seq := uint32(1); seq <= uint32(n) && time.Now().Before(end); seq++ {
		sent := time.Now()
		t := c.mono(sent)
		if err := c.send(Frame{Type: TypePing, Seq: seq, T: t}, nil); err != nil {
			return err
		}
		r.PingsSent++
		wait := sent.Add(pingTimeout)
		cut := wait.After(end)
		if cut {
			wait = end
		}
		answered := false
		for !answered {
			f, at, err := c.recv(wait)
			if err == errWait {
				break
			}
			if err != nil {
				return err
			}
			switch {
			case f.Type == TypePong && f.Seq == seq && f.T == t:
				rtts = append(rtts, float64(c.mono(at)-f.T)/1e6)
				answered = true
			case f.Type == TypePong && f.Seq < seq:
				// The late answer to a ping already counted as lost.
			default:
				return unexpected("ping", f)
			}
		}
		if !answered {
			if cut {
				break // the window closed while it was out
			}
			lost++
			if lost > n/2 {
				r.PingsAnswered = len(rtts)
				return ErrTooMuchLoss
			}
			continue
		}
		if int(seq) < n {
			next := time.Now().Add(pingInterval)
			if next.After(end) {
				next = end
			}
			if err := c.idle(next, "ping"); err != nil {
				return err
			}
		}
	}
	r.PingsAnswered = len(rtts)
	if len(rtts) < MinPings {
		return ErrTooMuchLoss
	}
	r.LatencyMS, r.JitterMS = Median(rtts), Jitter(rtts)
	return nil
}

// idle waits until then, taking only late PONGs meanwhile.
func (c *client) idle(then time.Time, phase string) error {
	for {
		f, _, err := c.recv(then)
		if err == errWait {
			return nil
		}
		if err != nil {
			return err
		}
		if f.Type != TypePong {
			return unexpected(phase, f)
		}
	}
}

// download asks for n bytes and times them from the end of the first DATA
// frame to DONE, so that the time the first frame took to arrive (one round
// trip) is not counted against the line's throughput.
func (c *client) download(n uint32, r *Result) error {
	if err := c.send(Frame{Type: TypeDown, Bytes: n}, nil); err != nil {
		return err
	}
	var total int64
	var first, frames int
	var t0 time.Time
	for {
		f, at, err := c.recv(time.Time{})
		if err != nil {
			return err
		}
		switch f.Type {
		case TypePong:
			continue
		case TypeData:
			if frames == 0 {
				first, t0 = f.Len, at
			}
			frames++
			total += int64(f.Len)
			if total > int64(n) {
				return fmt.Errorf("%w: the machine sent more than was asked for", ErrProtocol)
			}
			continue
		case TypeDone:
		default:
			return unexpected("download", f)
		}
		if f.Sent != uint64(total) {
			return fmt.Errorf("%w: the machine says it sent %d bytes, %d arrived", ErrProtocol, f.Sent, total)
		}
		r.DownBytes = total
		counted := int64(f.Sent) - int64(first)
		if us := at.Sub(t0).Microseconds(); frames > 1 && counted > 0 && us > 0 {
			r.DownMbps = float64(counted) * 8 / float64(us)
		} else if f.Elapsed > 0 {
			// Too little came to time it here: the machine's own figure.
			r.DownMbps = float64(f.Sent) * 8 / float64(f.Elapsed)
		}
		return nil
	}
}

// upload sends up to n bytes of random data, for at most upDuration, and
// takes the machine's measure of them.
func (c *client) upload(n uint32, r *Result) error {
	if err := c.send(Frame{Type: TypeUp, Bytes: n}, nil); err != nil {
		return err
	}
	frame := make([]byte, 3+MaxDataLen)
	frame[0] = TypeData
	rand.Read(frame[3:])
	stop := time.Now().Add(upDuration)
	var sent int64
	for sent < int64(n) && time.Now().Before(stop) {
		size := int(min(int64(MaxDataLen), int64(n)-sent))
		frame[1], frame[2] = byte(size>>8), byte(size)
		if _, err := c.conn.Write(frame[:3+size]); err != nil {
			// The machine may have said why before it stopped reading.
			if f, _, rerr := c.recv(time.Now().Add(100 * time.Millisecond)); rerr != nil && rerr != errWait {
				return rerr
			} else if rerr == nil && f.Type != TypePong {
				return unexpected("upload", f)
			}
			return c.fail(err)
		}
		sent += int64(size)
		// Nothing but a late PONG or an ERR may come meanwhile.
		select {
		case got := <-c.frames:
			if got.err != nil {
				return c.fail(got.err)
			}
			if got.f.Type == TypeErr {
				return &RemoteError{got.f.Code, got.f.Reason}
			}
			if got.f.Type != TypePong {
				return unexpected("upload", got.f)
			}
		default:
		}
	}
	r.UpBytes = sent
	if err := c.send(Frame{Type: TypeEnd, Sent: uint64(sent)}, nil); err != nil {
		return err
	}
	for {
		f, _, err := c.recv(time.Time{})
		if err != nil {
			return err
		}
		switch f.Type {
		case TypePong:
			continue
		case TypeUpResult:
			if f.Elapsed > 0 {
				r.UpMbps = float64(f.Counted) * 8 / float64(f.Elapsed)
			}
			return nil
		default:
			return unexpected("upload", f)
		}
	}
}

func (c *client) bye() error {
	if err := c.send(Frame{Type: TypeBye}, nil); err != nil {
		return err
	}
	for {
		f, _, err := c.recv(time.Time{})
		if err != nil {
			return err
		}
		switch f.Type {
		case TypePong:
		case TypeBye:
			return nil
		default:
			return unexpected("goodbye", f)
		}
	}
}

// Median of xs; 0 for none.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// Jitter is the mean absolute difference between consecutive samples; 0 for
// fewer than two.
func Jitter(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for i := 1; i < len(xs); i++ {
		sum += math.Abs(xs[i] - xs[i-1])
	}
	return sum / float64(len(xs)-1)
}
