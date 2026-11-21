package pretest

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// defaultLimits are the platform's caps for one test (its
// leasetoken.DefaultTestLimits), for the tests ported from the Miner.
func defaultLimits() Limits {
	return Limits{Pings: 20, MaxDownBytes: 10_000_000, MaxUpBytes: 10_000_000, MaxSeconds: 10}
}

// Connect's measurement over distant, slow lines, against the Miner's own
// server: it ends well inside the ticket's 10 s, with figures that match
// the line.
func TestMeasureShaped(t *testing.T) {
	for _, c := range []struct {
		name     string
		rtt      time.Duration
		down, up float64
	}{
		{"150ms 4/2", 150 * time.Millisecond, 4e6, 2e6},
		{"300ms 4/2", 300 * time.Millisecond, 4e6, 2e6},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client, server := link(t, c.rtt, c.down, c.up)
			lim := defaultLimits()
			start := time.Now()
			done := serve(server, lim)
			m, err := Measure(context.Background(), client, lim)
			took := time.Since(start)
			s := <-done
			if err != nil || s.err != nil || !s.rep.Complete {
				t.Fatalf("after %s: client %v, server %v", took, err, s.err)
			}
			if took > 9500*time.Millisecond {
				t.Errorf("took %s", took)
			}
			rtt := float64(c.rtt.Milliseconds())
			if len(m.Result.RTTs) < MinPings || m.LatencyMS < rtt || m.LatencyMS > rtt+40 || m.JitterMS > 20 {
				t.Errorf("%d pings, latency %.1f ms, jitter %.1f ms over a %s line", len(m.Result.RTTs), m.LatencyMS, m.JitterMS, c.rtt)
			}
			if math.Abs(m.DownMbps-c.down/1e6) > c.down/1e6*0.4 || math.Abs(m.UpMbps-c.up/1e6) > c.up/1e6*0.4 {
				t.Errorf("down %.2f, up %.2f Mbit/s over a %.0f/%.0f line", m.DownMbps, m.UpMbps, c.down/1e6, c.up/1e6)
			}
			t.Logf("%s: %d pings, %.1f ms, jitter %.1f, down %.2f, up %.2f", took.Round(10*time.Millisecond),
				len(m.Result.RTTs), m.LatencyMS, m.JitterMS, m.DownMbps, m.UpMbps)
		})
	}
}

func TestMeasureLoopback(t *testing.T) {
	client, server := pair(t)
	lim := defaultLimits()
	done := serve(server, lim)
	m, err := Measure(context.Background(), client, lim)
	if s := <-done; err != nil || s.err != nil {
		t.Fatalf("client %v, server %v", err, s.err)
	}
	if len(m.Result.RTTs) != lim.Pings || m.LatencyMS <= 0 || m.LatencyMS > 50 || m.DownMbps < 50 || m.UpMbps < 50 {
		t.Errorf("%+v", m)
	}
}

// A ticket that leaves fewer than MinPings round trips is too few to rate.
func TestMeasureTooFewPings(t *testing.T) {
	client, server := pair(t)
	lim := Limits{Pings: 2, MaxDownBytes: 1000, MaxUpBytes: 1000, MaxSeconds: 5}
	done := serve(server, lim)
	if _, err := Measure(context.Background(), client, lim); !errors.Is(err, ErrTooFewPings) {
		t.Errorf("got %v, want ErrTooFewPings", err)
	}
	<-done
}

// Out of time is ErrTimeout, whether the machine says so (ERR 3) or the
// ticket's time runs out here first.
func TestMeasureTimeout(t *testing.T) {
	client, server := pair(t)
	done := serve(server, Limits{Pings: 20, MaxDownBytes: 10_000_000, MaxUpBytes: 10_000_000, MaxSeconds: 0})
	if _, err := Measure(context.Background(), client, defaultLimits()); !errors.Is(err, ErrTimeout) {
		t.Errorf("machine's deadline: %v", err)
	}
	<-done

	client, _ = pair(t) // a machine that never answers
	start := time.Now()
	_, err := Measure(context.Background(), client, Limits{Pings: 20, MaxSeconds: 1})
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 2*time.Second {
		t.Errorf("silent machine: %v after %s", err, time.Since(start))
	}
}

func TestMeasureStopped(t *testing.T) {
	client, server := pair(t)
	done := serve(server, Limits{Pings: 1, MaxDownBytes: 1000, MaxUpBytes: 1000, MaxSeconds: 5})
	_, err := Measure(context.Background(), client, defaultLimits())
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeOverLimit || !strings.Contains(err.Error(), "machine stopped the test") ||
		errors.Is(err, ErrTimeout) {
		t.Errorf("got %v", err)
	}
	<-done
}

func TestMeasureCancel(t *testing.T) {
	client, _ := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Measure(ctx, client, defaultLimits()); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
		t.Errorf("got %v", err)
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
		if m := Median(c.xs); m != c.median {
			t.Errorf("Median(%v) = %v, want %v", c.xs, m, c.median)
		}
		if j := Jitter(c.xs); math.Abs(j-c.jitter) > 1e-9 {
			t.Errorf("Jitter(%v) = %v, want %v", c.xs, j, c.jitter)
		}
	}
	// Median does not reorder its argument: jitter follows arrival order.
	xs := []float64{3, 1, 2}
	Median(xs)
	if xs[0] != 3 {
		t.Error("Median sorted its argument")
	}
}

func TestClamp(t *testing.T) {
	got := Limits{Pings: 99, MaxDownBytes: 1 << 40, MaxUpBytes: -1, MaxSeconds: 60}.Clamp()
	if got != (Limits{MinerMaxPings, MinerMaxDownBytes, 0, MinerMaxSeconds}) {
		t.Errorf("%+v", got)
	}
}
