package pretest_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"math"
	"net"
	"testing"
	"time"

	"github.com/nodeexhq/connect/internal/pretest"
	"github.com/nodeexhq/connect/internal/pretest/fake"
)

var ticket = pretest.Limits{Pings: 20, MaxDownBytes: 10_000_000, MaxUpBytes: 10_000_000, MaxSeconds: 10}

// run runs the client against a fake machine over an in-memory connection.
func run(t *testing.T, lim pretest.Limits, o fake.Options) (pretest.Result, error, time.Duration) {
	t.Helper()
	c, s := net.Pipe()
	defer c.Close()
	served := make(chan error, 1)
	go func() {
		defer s.Close()
		served <- fake.Serve(s, o)
	}()
	start := time.Now()
	r, err := pretest.Run(context.Background(), c, lim)
	took := time.Since(start)
	c.Close()
	<-served
	return r, err, took
}

func TestFastMachine(t *testing.T) {
	r, err, took := run(t, ticket, fake.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.PingsSent != 20 || r.PingsAnswered != 20 {
		t.Errorf("pings %d sent, %d answered", r.PingsSent, r.PingsAnswered)
	}
	if r.DownBytes != ticket.MaxDownBytes || r.UpBytes != ticket.MaxUpBytes {
		t.Errorf("moved %d down, %d up; the ticket allows %d and %d", r.DownBytes, r.UpBytes, ticket.MaxDownBytes, ticket.MaxUpBytes)
	}
	if r.LatencyMS <= 0 || r.LatencyMS > 20 || r.DownMbps < 50 || r.UpMbps < 50 {
		t.Errorf("an in-memory machine measured %+v", r)
	}
	// 19 pauses of 50 ms between pings, then the data.
	if took < 19*50*time.Millisecond || took > 5*time.Second {
		t.Errorf("took %v", took)
	}
}

func TestSlowLossyMachine(t *testing.T) {
	defer pretest.SetTimings(10*time.Millisecond, 200*time.Millisecond, time.Second)()
	lim := pretest.Limits{Pings: 12, MaxDownBytes: 400_000, MaxUpBytes: 2_000_000, MaxSeconds: 10}
	r, err, _ := run(t, lim, fake.Options{
		PongDelay: 30 * time.Millisecond,
		// Every fourth ping is lost, and answered long after the client
		// has moved on: the late answer must not count or confuse it.
		Drop:      func(seq uint32) bool { return seq%4 == 0 },
		LatePongs: 250 * time.Millisecond,
		DownRate:  1_000_000, // 8 Mbit/s
		UpRate:    500_000,   // 4 Mbit/s
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.PingsSent != 12 || r.PingsAnswered != 9 {
		t.Errorf("pings %d sent, %d answered; want 12 and 9", r.PingsSent, r.PingsAnswered)
	}
	if r.LatencyMS < 30 || r.LatencyMS > 60 {
		t.Errorf("latency %.1f ms, want about 30", r.LatencyMS)
	}
	if math.Abs(r.DownMbps-8) > 1.5 {
		t.Errorf("down %.2f Mbit/s, want about 8", r.DownMbps)
	}
	// The upload stops at 1 s here: 2 MB would take 4 s at 4 Mbit/s.
	if math.Abs(r.UpMbps-4) > 1 || r.UpBytes > lim.MaxUpBytes {
		t.Errorf("up %.2f Mbit/s, %d bytes; want about 4 Mbit/s", r.UpMbps, r.UpBytes)
	}
}

func TestTooMuchLoss(t *testing.T) {
	defer pretest.SetTimings(10*time.Millisecond, 100*time.Millisecond, time.Second)()
	_, err, took := run(t, pretest.Limits{Pings: 10, MaxDownBytes: 1000, MaxUpBytes: 1000, MaxSeconds: 10},
		fake.Options{Drop: func(seq uint32) bool { return seq%2 == 1 || seq > 6 }})
	if !errors.Is(err, pretest.ErrTooMuchLoss) {
		t.Fatalf("got %v, want ErrTooMuchLoss", err)
	}
	// It gives up at the sixth loss (ping 9) instead of waiting out ping 10.
	if took > 1500*time.Millisecond {
		t.Errorf("took %v to give up", took)
	}
}

func TestTimeLimit(t *testing.T) {
	// 10 MB at 1 MB/s cannot be fetched in the ticket's 2 seconds.
	_, err, took := run(t, pretest.Limits{Pings: 2, MaxDownBytes: 10_000_000, MaxUpBytes: 1000, MaxSeconds: 2},
		fake.Options{DownRate: 1_000_000})
	if !errors.Is(err, pretest.ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
	if took > 3*time.Second {
		t.Errorf("took %v, the ticket allows 2 s", took)
	}
}

func TestMachineError(t *testing.T) {
	for _, phase := range []byte{pretest.TypePing, pretest.TypeDown, pretest.TypeUp, pretest.TypeBye} {
		_, err, _ := run(t, pretest.Limits{Pings: 1, MaxDownBytes: 1000, MaxUpBytes: 1000, MaxSeconds: 5},
			fake.Options{FailOn: phase, FailCode: 3, FailReason: "budget"})
		var re *pretest.RemoteError
		if !errors.As(err, &re) || re.Code != 3 || re.Reason != "budget" {
			t.Errorf("ERR on 0x%02x: got %v", phase, err)
		}
	}
}

func TestCancel(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	go fake.Serve(s, fake.Options{PongDelay: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := pretest.Run(ctx, c, ticket); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestBadLimits(t *testing.T) {
	c, _ := net.Pipe()
	defer c.Close()
	if _, err := pretest.Run(context.Background(), c, pretest.Limits{Pings: 0, MaxSeconds: 10}); err == nil {
		t.Fatal("a ticket with no pings ran")
	}
}

func TestMedianJitter(t *testing.T) {
	cases := []struct {
		xs             []float64
		median, jitter float64
	}{
		{nil, 0, 0},
		{[]float64{40}, 40, 0},
		{[]float64{30, 50}, 40, 20},
		{[]float64{38, 42, 36, 40, 90}, 40, (4 + 6 + 4 + 50) / 4.0},
		{[]float64{10, 10, 10, 10}, 10, 0},
	}
	for _, c := range cases {
		if m := pretest.Median(c.xs); m != c.median {
			t.Errorf("Median(%v) = %v, want %v", c.xs, m, c.median)
		}
		if j := pretest.Jitter(c.xs); math.Abs(j-c.jitter) > 1e-9 {
			t.Errorf("Jitter(%v) = %v, want %v", c.xs, j, c.jitter)
		}
	}
	// Median does not reorder its argument: jitter follows arrival order.
	xs := []float64{3, 1, 2}
	pretest.Median(xs)
	if xs[0] != 3 {
		t.Error("Median sorted its argument")
	}
}

func TestFrameEncoding(t *testing.T) {
	frames := []pretest.Frame{
		{Type: pretest.TypePing, Seq: 7, T: 1 << 40},
		{Type: pretest.TypePong, Seq: 7, T: 1 << 40},
		{Type: pretest.TypeDown, Bytes: 10_000_000},
		{Type: pretest.TypeDone, Sent: 9, Elapsed: 10},
		{Type: pretest.TypeUp, Bytes: 5},
		{Type: pretest.TypeEnd, Sent: 5},
		{Type: pretest.TypeUpResult, Received: 1, Counted: 2, Elapsed: 3},
		{Type: pretest.TypeBye},
		{Type: pretest.TypeErr, Code: 9, Reason: "busy"},
	}
	var b []byte
	for _, f := range frames {
		b = pretest.AppendFrame(b, f, nil)
	}
	b = pretest.AppendFrame(b, pretest.Frame{Type: pretest.TypeData}, []byte("hello"))
	// The wire format, byte for byte, for two of them.
	if !bytes.HasPrefix(b, []byte{0x01, 0, 0, 0, 7, 0, 0, 1, 0, 0, 0, 0, 0}) {
		t.Errorf("PING encoded as % x", b[:13])
	}
	if !bytes.HasSuffix(b, []byte{0x11, 0, 5, 'h', 'e', 'l', 'l', 'o'}) {
		t.Errorf("DATA encoded as % x", b[len(b)-8:])
	}
	r := bufio.NewReader(bytes.NewReader(b))
	for _, want := range frames {
		got, err := pretest.ReadFrame(r)
		if err != nil || got != want {
			t.Errorf("read %+v, %v; want %+v", got, err, want)
		}
	}
	if got, err := pretest.ReadFrame(r); err != nil || got.Type != pretest.TypeData || got.Len != 5 {
		t.Errorf("DATA read as %+v, %v", got, err)
	}
	if _, err := pretest.ReadFrame(bufio.NewReader(bytes.NewReader([]byte{0x55}))); !errors.Is(err, pretest.ErrProtocol) {
		t.Errorf("unknown type: %v", err)
	}
}
