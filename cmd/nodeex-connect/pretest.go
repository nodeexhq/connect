package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/nodeexhq/connect/internal/pretest"
	"github.com/nodeexhq/miner/gate"
	"github.com/nodeexhq/miner/overlay"
)

// The connection test before an order. The website reserves a machine of the
// kind the renter wants and hands the reservation to Connect, which reaches
// the machine over the overlay as a lease would, measures the connection
// against the gate's "test" service, and reports it. The platform rates it,
// and the renter goes back to the browser to order, try another machine, or
// give up. Nothing is charged for a test.

// reservationTest is the test ticket the platform issues for a reservation.
type reservationTest struct {
	TicketID   string         `json:"ticket_id"`
	Token      string         `json:"token"`
	ExpiresAt  time.Time      `json:"expires_at"`
	Service    string         `json:"service"`
	ControlURL string         `json:"control_url"`
	AuthKey    string         `json:"auth_key"`
	NodeAddr   string         `json:"node_addr"`
	GatePort   int            `json:"gate_port"`
	Limits     pretest.Limits `json:"limits"`
}

type testResult struct {
	TicketID  string  `json:"ticket_id"`
	LatencyMS float64 `json:"latency_ms"`
	JitterMS  float64 `json:"jitter_ms"`
	DownMbps  float64 `json:"down_mbps"`
	UpMbps    float64 `json:"up_mbps"`
	Direct    bool    `json:"direct"`
	Relay     string  `json:"relay"`
	Verdict   string  `json:"verdict,omitempty"`
}

type reservation struct {
	ID     string           `json:"id"`
	Status string           `json:"status"`
	Result *testResult      `json:"result"`
	Test   *reservationTest `json:"test"`
}

// overlayNet is what a test needs of the overlay network.
type overlayNet struct {
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	probe func(ctx context.Context, peer netip.Addr) (overlay.Path, error)
	close func() error
}

type joinFunc func(ctx context.Context, c overlay.Config) (overlayNet, error)

func joinOverlay(ctx context.Context, c overlay.Config) (overlayNet, error) {
	srv, err := overlay.Start(ctx, c, log.Printf)
	if err != nil {
		return overlayNet{}, err
	}
	return overlayNet{
		dial:  srv.Dial,
		probe: func(ctx context.Context, peer netip.Addr) (overlay.Path, error) { return overlay.Probe(ctx, srv, peer) },
		close: srv.Close,
	}, nil
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.BoolVar(&eventsOn, "events", false, "write progress as JSON lines on standard output, for the app")
	// Accept the reservation ID before or after the flags.
	var id string
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		id, args = args[0], args[1:]
	}
	fs.Parse(args)
	if id == "" {
		id = fs.Arg(0)
	}
	if id == "" {
		return errors.New("usage: nodeex-connect test RESERVATION_ID [-events]")
	}
	s, err := loadSession(*dir)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// A ticket lives five minutes; a test takes seconds.
	ctx, cancelTimeout := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelTimeout()
	out, err := runTest(ctx, s, id, joinOverlay)
	if err != nil {
		return err
	}
	emit("test_result", map[string]any{
		"latency_ms": out.LatencyMS, "jitter_ms": out.JitterMS, "down_mbps": out.DownMbps, "up_mbps": out.UpMbps,
		"direct": out.Direct, "relay": out.Relay, "verdict": out.Verdict,
	})
	if eventsOn {
		log.Print(testSummary(out))
	} else {
		fmt.Println(testSummary(out))
	}
	return nil
}

// runTest runs one connection test for a reservation and returns the result
// as the platform recorded and rated it.
func runTest(ctx context.Context, s session, id string, join joinFunc) (testResult, error) {
	if !uuidRE.MatchString(id) {
		return testResult{}, errors.New("that is not a reservation ID; start the test from the website")
	}
	id = strings.ToLower(id)
	path := "/v1/reservations/" + id

	var res reservation
	if err := call(ctx, s, "POST", path+"/test", nil, &res); err != nil {
		return testResult{}, reservationProblem(err)
	}
	t := res.Test
	if t == nil || t.Service != "test" || t.TicketID == "" || t.Token == "" {
		return testResult{}, errors.New("the platform did not issue a usable test ticket; try again")
	}
	node, err := netip.ParseAddr(t.NodeAddr)
	if err != nil || t.GatePort <= 0 || t.GatePort > 65535 {
		return testResult{}, errors.New("platform returned an invalid machine address")
	}

	// A fresh, ephemeral identity on the overlay for this test alone.
	stateDir, err := os.MkdirTemp("", "nodeex-test-")
	if err != nil {
		return testResult{}, err
	}
	defer os.RemoveAll(stateDir)
	log.Printf("joining the NodeEx network")
	jctx, jcancel := context.WithTimeout(ctx, time.Minute)
	ov, err := join(jctx, overlay.Config{
		ControlURL: t.ControlURL, AuthKey: t.AuthKey,
		Hostname: "t-" + t.TicketID[:min(8, len(t.TicketID))], StateDir: stateDir,
	})
	jcancel()
	if err != nil {
		if ctx.Err() != nil {
			return testResult{}, ctx.Err()
		}
		return testResult{}, fmt.Errorf("could not join the NodeEx network: %w", err)
	}
	defer ov.close()

	log.Printf("reaching the machine")
	conn, err := ov.dial(ctx, "tcp", netip.AddrPortFrom(node, uint16(t.GatePort)).String())
	if err != nil {
		if ctx.Err() != nil {
			return testResult{}, ctx.Err()
		}
		return testResult{}, fmt.Errorf("could not reach the machine: %w", err)
	}
	defer conn.Close()
	if err := gate.Dial(conn, "test", t.Token); err != nil {
		return testResult{}, gateProblem(err)
	}

	emit("test_started", nil)
	log.Printf("measuring the connection (at most %d s)", t.Limits.MaxSeconds)
	// Ask for the path while the test's traffic flows, and again right
	// after: the overlay may move from a relay to a direct path once
	// there is traffic.
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	type probed struct {
		p   overlay.Path
		err error
	}
	during := make(chan probed, 1)
	go func() {
		c, cancel := context.WithTimeout(pctx, 10*time.Second)
		defer cancel()
		p, err := ov.probe(c, node)
		during <- probed{p, err}
	}()
	m, err := pretest.Run(ctx, conn, t.Limits)
	conn.Close()
	if err != nil {
		if ctx.Err() != nil {
			return testResult{}, ctx.Err()
		}
		return testResult{}, measureFailure(err)
	}
	actx, acancel := context.WithTimeout(ctx, 5*time.Second)
	path2, err := ov.probe(actx, node)
	acancel()
	if err != nil {
		if d := <-during; d.err == nil {
			path2, err = d.p, nil
		}
	}
	if err != nil {
		// Unknown counts as relayed: better to warn of lag than to promise
		// a smoothness the path may not give.
		log.Printf("could not tell whether the path is direct: %v", err)
		path2 = overlay.Path{}
	}
	log.Printf("path: %s", path2)

	r := testResult{
		TicketID:  t.TicketID,
		LatencyMS: min(m.LatencyMS, 60_000), JitterMS: min(m.JitterMS, 60_000),
		DownMbps: min(m.DownMbps, 100_000), UpMbps: min(m.UpMbps, 100_000),
		Direct: path2.Direct,
	}
	if !r.Direct {
		r.Relay = path2.Relay[:min(16, len(path2.Relay))]
	}
	var rated reservation
	if err := call(ctx, s, "POST", path+"/result", r, &rated); err != nil {
		return testResult{}, reservationProblem(err)
	}
	if rated.Result == nil || rated.Result.Verdict == "" {
		return testResult{}, errors.New("the platform did not rate the test")
	}
	return *rated.Result, nil
}

// slowMessage is what a renter hears when the connection was too poor to
// finish the test: that is a verdict too, if one without figures.
const slowMessage = "This connection is too slow or unstable to measure; the remote desktop would likely stutter. Try another machine."

// testFailure is a test that the machine accepted but that did not finish.
// Reason is "slow" when the connection was too poor for it, "error"
// otherwise. No result is reported to the platform for it.
type testFailure struct {
	Reason string
	Err    error
}

func (f *testFailure) Error() string {
	if f.Reason == "slow" {
		return slowMessage
	}
	return f.Err.Error()
}

func (f *testFailure) Unwrap() error { return f.Err }

func measureFailure(err error) error {
	if errors.Is(err, pretest.ErrTimeout) || errors.Is(err, pretest.ErrTooMuchLoss) {
		return &testFailure{"slow", err}
	}
	return &testFailure{"error", err}
}

// reservationProblem says plainly why the platform refused.
func reservationProblem(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == http.StatusUnauthorized:
		return errors.New("not signed in, or the session has expired; sign in again")
	case ae.Code == "reservation_expired":
		return errors.New("this reservation has expired; reserve a machine again on the website")
	case ae.Code == "reservation_ended":
		return errors.New("this reservation has already ended: it was ordered or given up")
	case ae.Code == "ticket_limit":
		return errors.New("this machine has been tested as many times as a reservation allows; order it, or try another machine")
	case ae.Code == "not_found":
		return errors.New("this reservation is not in the signed-in account")
	case ae.Code == "overlay_unavailable":
		return errors.New("connection tests are not available right now; try again in a few minutes")
	case ae.Code == "result_recorded":
		return errors.New("a result has already been reported for this test; run the test again")
	}
	return err
}

// gateProblem says plainly why the machine refused the test.
func gateProblem(err error) error {
	var refused *gate.Refused
	if !errors.As(err, &refused) {
		return fmt.Errorf("could not reach the machine's test: %w", err)
	}
	switch refused.Reason {
	case "busy":
		return errors.New("the machine is busy: it is in use or was tested a moment ago; try again in a minute, or try another machine")
	case "token-expired":
		return errors.New("the test ticket expired before the machine was reached; run the test again")
	case "token-used":
		return errors.New("this test ticket has already been used; run the test again")
	case "unauthorized":
		return errors.New("the machine did not accept the test ticket; run the test again, or try another machine")
	}
	return fmt.Errorf("the machine refused the test (%s); try another machine", refused.Reason)
}

var verdictWords = map[string]string{
	"smooth":  "smooth: remote desktop should feel responsive",
	"ok":      "ok: usable, though remote desktop may not always feel instant",
	"may_lag": "may lag: remote desktop may stutter on this connection",
}

// testSummary is the result in one line.
func testSummary(r testResult) string {
	path := "Direct connection"
	if !r.Direct {
		path = "Relayed connection"
		if r.Relay != "" {
			path += " (via " + r.Relay + ")"
		}
	}
	verdict := verdictWords[r.Verdict]
	if verdict == "" {
		verdict = r.Verdict
	}
	return fmt.Sprintf("%s, %.0f ms, jitter %.0f ms, %s Mbit/s down, %s up — %s",
		path, r.LatencyMS, r.JitterMS, mbps(r.DownMbps), mbps(r.UpMbps), verdict)
}

func mbps(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.0f", v)
}
