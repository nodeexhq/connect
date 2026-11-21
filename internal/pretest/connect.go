package pretest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"slices"
	"time"
)

// MinPings is the fewest answered pings a test can be rated on.
const MinPings = 3

var (
	// ErrTooFewPings: fewer than MinPings round trips fit in the ping
	// window.
	ErrTooFewPings = errors.New("too few of the test's pings were answered: the connection to this machine is too slow")
	// ErrTimeout: the test did not finish within the ticket's time.
	ErrTimeout = errors.New("the test did not finish in the time allowed: the connection to this machine is too slow")
)

// Measurement is what Connect reports of a test.
type Measurement struct {
	LatencyMS float64 // median round trip
	JitterMS  float64 // mean change in round trip between consecutive pings
	DownMbps  float64 // machine to this computer
	UpMbps    float64 // this computer to the machine
	Result    Result  // everything the client measured
}

// Measure runs the whole test the ticket allows on conn, which the gate has
// just accepted for the test service, and sums it up. It ends within the
// ticket's max_seconds. A test that runs out of time, or of pings, is
// ErrTimeout or ErrTooFewPings; one the machine stopped is an *Error.
func Measure(ctx context.Context, conn net.Conn, lim Limits) (Measurement, error) {
	if lim.MaxSeconds < 1 {
		return Measurement{}, fmt.Errorf("the test ticket allows no time: %+v", lim)
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(lim.MaxSeconds)*time.Second)
	defer cancel()
	res, err := Run(tctx, conn, OptionsFor(lim))
	var e *Error
	switch {
	case ctx.Err() != nil:
		return Measurement{}, ctx.Err()
	case err == nil:
	case errors.As(err, &e) && e.Code == CodeDeadline,
		tctx.Err() != nil, errors.Is(err, os.ErrDeadlineExceeded):
		return Measurement{Result: res}, fmt.Errorf("%w (%w)", ErrTimeout, err)
	case errors.As(err, &e):
		return Measurement{Result: res}, fmt.Errorf("the machine stopped the test (%s): %w", e.Reason, err)
	default:
		return Measurement{Result: res}, err
	}
	if len(res.RTTs) < MinPings {
		return Measurement{Result: res}, ErrTooFewPings
	}
	ms := make([]float64, len(res.RTTs))
	for i, d := range res.RTTs {
		ms[i] = float64(d) / 1e6
	}
	return Measurement{
		LatencyMS: Median(ms), JitterMS: Jitter(ms),
		DownMbps: res.DownMbps(), UpMbps: res.UpMbps(), Result: res,
	}, nil
}

// Median of xs; 0 for none. With an even count, the mean of the middle two.
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
