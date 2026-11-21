package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nodeexhq/connect/internal/pretest"
	"github.com/nodeexhq/miner/overlay"
)

const testReservation = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

// testPlatform is the platform's side of a test: it issues a ticket, or
// refuses with ticketErr, and rates the result it is sent as verdict.
type testPlatform struct {
	ticketStatus int
	ticketErr    string
	resultStatus int
	resultErr    string
	verdict      string

	mu     sync.Mutex
	posted map[string]any
}

func (p *testPlatform) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refuse := func(status int, code string) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":%q,"message":"refused"}}`, code)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			refuse(http.StatusUnauthorized, "unauthorized")
			return
		}
		base := "/v1/reservations/" + testReservation
		switch {
		case r.Method == "POST" && r.URL.Path == base+"/test":
			if p.ticketErr != "" {
				refuse(p.ticketStatus, p.ticketErr)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"`+testReservation+`","status":"live","test":{"ticket_id":"0123456789abcdef0123456789abcdef",
				"token":"TOKEN","expires_at":"2026-11-21T12:05:00Z","service":"test","control_url":"https://hs.example",
				"auth_key":"KEY","node_addr":"100.64.0.7","gate_port":7000,
				"limits":{"pings":5,"max_down_bytes":200000,"max_up_bytes":200000,"max_seconds":10}}}`)
		case r.Method == "POST" && r.URL.Path == base+"/result":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			p.mu.Lock()
			p.posted = body
			p.mu.Unlock()
			if p.resultErr != "" {
				refuse(p.resultStatus, p.resultErr)
				return
			}
			res := map[string]any{"id": testReservation, "status": "live", "result": map[string]any{
				"ticket_id": body["ticket_id"], "latency_ms": 38.2, "jitter_ms": 4.1, "down_mbps": 52.3, "up_mbps": 18.4,
				"direct": body["direct"], "relay": body["relay"], "verdict": p.verdict,
			}}
			json.NewEncoder(w).Encode(res)
		default:
			refuse(http.StatusNotFound, "not_found")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testOverlay is the overlay with one machine on it, whose gate answers
// with reply and then runs a fake test.
type testOverlay struct {
	reply string
	// machine is the limits the machine's test server keeps; the ticket's
	// when nil.
	machine *pretest.Limits
	path    overlay.Path
	cfg     overlay.Config
	closed  bool
	mu      sync.Mutex
	gotTok  string
	dialed  string
}

func (o *testOverlay) join(ctx context.Context, c overlay.Config) (overlayNet, error) {
	o.cfg = c
	return overlayNet{
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			o.dialed = addr
			// A real socket, as the overlay gives: the test's flow control
			// expects one.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			go func() {
				defer ln.Close()
				machine, err := ln.Accept()
				if err != nil {
					return
				}
				defer machine.Close()
				line, err := bufio.NewReader(machine).ReadString('\n')
				if err != nil {
					return
				}
				o.mu.Lock()
				o.gotTok = strings.TrimSpace(line)
				o.mu.Unlock()
				fmt.Fprintf(machine, "%s\n", o.reply)
				if o.reply == "OK" {
					lim := pretest.Limits{Pings: 5, MaxDownBytes: 200000, MaxUpBytes: 200000, MaxSeconds: 10}
					if o.machine != nil {
						lim = *o.machine
					}
					pretest.Serve(machine, lim, time.Now())
				}
			}()
			return net.Dial("tcp", ln.Addr().String())
		},
		probe: func(ctx context.Context, peer netip.Addr) (overlay.Path, error) { return o.path, nil },
		close: func() error { o.closed = true; return nil },
	}, nil
}

func TestRunTest(t *testing.T) {
	for _, c := range []struct {
		name    string
		path    overlay.Path
		verdict string
	}{
		{"direct", overlay.Path{Direct: true, Endpoint: "1.2.3.4:41641"}, "smooth"},
		{"relayed", overlay.Path{Relay: "fra"}, "may_lag"},
		{"ok", overlay.Path{Direct: true}, "ok"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &testPlatform{verdict: c.verdict}
			srv := p.serve(t)
			o := &testOverlay{reply: "OK", path: c.path}
			got, err := runTest(context.Background(), session{API: srv.URL, Token: "tok"}, strings.ToUpper(testReservation), o.join)
			if err != nil {
				t.Fatal(err)
			}
			// The platform's verdict and figures come back as it sent them.
			if got.Verdict != c.verdict || got.LatencyMS != 38.2 || got.DownMbps != 52.3 || got.Direct != c.path.Direct {
				t.Errorf("got %+v", got)
			}
			o.mu.Lock()
			gotTok := o.gotTok
			o.mu.Unlock()
			if gotTok != "NODEEX1 test TOKEN" || o.dialed != "100.64.0.7:7000" {
				t.Errorf("gate got %q at %q", gotTok, o.dialed)
			}
			if o.cfg.Hostname != "t-01234567" || o.cfg.ControlURL != "https://hs.example" || o.cfg.AuthKey != "KEY" {
				t.Errorf("joined as %+v", o.cfg)
			}
			if _, err := os.Stat(o.cfg.StateDir); !errors.Is(err, os.ErrNotExist) || !o.closed {
				t.Errorf("overlay closed %v, state dir left: %v", o.closed, err)
			}
			p.mu.Lock()
			posted := p.posted
			p.mu.Unlock()
			for _, k := range []string{"ticket_id", "latency_ms", "jitter_ms", "down_mbps", "up_mbps", "direct", "relay"} {
				if _, ok := posted[k]; !ok {
					t.Errorf("result posted without %s: %v", k, posted)
				}
			}
			if posted["ticket_id"] != "0123456789abcdef0123456789abcdef" || posted["direct"] != c.path.Direct {
				t.Errorf("posted %v", posted)
			}
			if lat, _ := posted["latency_ms"].(float64); lat <= 0 {
				t.Errorf("posted latency %v", posted["latency_ms"])
			}
			want := ""
			if !c.path.Direct {
				want = c.path.Relay
			}
			if posted["relay"] != want {
				t.Errorf("posted relay %q, want %q", posted["relay"], want)
			}
		})
	}
}

func TestRunTestRefusals(t *testing.T) {
	cases := []struct {
		name string
		p    *testPlatform
		gate string
		want string
	}{
		{"expired", &testPlatform{ticketStatus: 410, ticketErr: "reservation_expired"}, "OK", "has expired"},
		{"ended", &testPlatform{ticketStatus: 409, ticketErr: "reservation_ended"}, "OK", "already ended"},
		{"ticket limit", &testPlatform{ticketStatus: 429, ticketErr: "ticket_limit"}, "OK", "as many times as a reservation allows"},
		{"not found", &testPlatform{ticketStatus: 404, ticketErr: "not_found"}, "OK", "not in the signed-in account"},
		{"no overlay", &testPlatform{ticketStatus: 503, ticketErr: "overlay_unavailable"}, "OK", "not available right now"},
		{"result twice", &testPlatform{resultStatus: 409, resultErr: "result_recorded", verdict: "ok"}, "OK", "already been reported"},
		{"busy", &testPlatform{verdict: "ok"}, "ERR busy", "machine is busy"},
		{"token expired", &testPlatform{verdict: "ok"}, "ERR token-expired", "ticket expired"},
		{"token used", &testPlatform{verdict: "ok"}, "ERR token-used", "already been used"},
		{"unauthorized", &testPlatform{verdict: "ok"}, "ERR unauthorized", "did not accept the test ticket"},
		{"other", &testPlatform{verdict: "ok"}, "ERR protocol", "refused the test (protocol)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := c.p.serve(t)
			o := &testOverlay{reply: c.gate, path: overlay.Path{Direct: true}}
			_, err := runTest(context.Background(), session{API: srv.URL, Token: "tok"}, testReservation, o.join)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if o.cfg.StateDir != "" {
				if _, err := os.Stat(o.cfg.StateDir); !errors.Is(err, os.ErrNotExist) || !o.closed {
					t.Errorf("overlay closed %v, state dir left: %v", o.closed, err)
				}
			}
		})
	}
	// Signed out, and an ID that is not one.
	p := &testPlatform{}
	srv := p.serve(t)
	o := &testOverlay{reply: "OK"}
	if _, err := runTest(context.Background(), session{API: srv.URL, Token: "old"}, testReservation, o.join); err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Errorf("signed out: %v", err)
	}
	if _, err := runTest(context.Background(), session{API: srv.URL, Token: "tok"}, "../leases", o.join); err == nil || !strings.Contains(err.Error(), "not a reservation ID") {
		t.Errorf("bad id: %v", err)
	}
}

// A test the machine accepted but that could not finish is a failure with a
// reason, and no result is reported for it.
func TestRunTestFails(t *testing.T) {
	for _, c := range []struct {
		name   string
		lim    pretest.Limits
		reason string
		msg    string
	}{
		// The machine's own deadline comes first: ERR 3.
		{"out of time", pretest.Limits{Pings: 5, MaxDownBytes: 200000, MaxUpBytes: 200000, MaxSeconds: 0}, "slow", slowMessage},
		// It allows fewer pings than the ticket says: ERR 2.
		{"machine error", pretest.Limits{Pings: 1, MaxDownBytes: 200000, MaxUpBytes: 200000, MaxSeconds: 5}, "error", "machine stopped the test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &testPlatform{verdict: "ok"}
			srv := p.serve(t)
			o := &testOverlay{reply: "OK", machine: &c.lim, path: overlay.Path{Direct: true}}
			_, err := runTest(context.Background(), session{API: srv.URL, Token: "tok"}, testReservation, o.join)
			var f *testFailure
			if !errors.As(err, &f) || f.Reason != c.reason || !strings.Contains(f.Error(), c.msg) {
				t.Fatalf("got %v (%#v), want %s: %q", err, f, c.reason, c.msg)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.posted != nil {
				t.Errorf("a failed test was reported: %v", p.posted)
			}
		})
	}
}

func TestTestSummary(t *testing.T) {
	got := testSummary(testResult{LatencyMS: 38.2, JitterMS: 4.1, DownMbps: 52.3, UpMbps: 18.4, Direct: true, Verdict: "smooth"})
	if want := "Direct connection, 38 ms, jitter 4 ms, 52 Mbit/s down, 18 up — smooth: remote desktop should feel responsive"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	got = testSummary(testResult{LatencyMS: 180, JitterMS: 41, DownMbps: 3.24, UpMbps: 1.5, Relay: "fra", Verdict: "may_lag"})
	if !strings.HasPrefix(got, "Relayed connection (via fra), 180 ms, jitter 41 ms, 3.2 Mbit/s down, 1.5 up — may lag") {
		t.Errorf("got %q", got)
	}
}

// The ticket's limits come through to the protocol as the platform set them.
func TestTicketLimitsDecode(t *testing.T) {
	var v reservationTest
	json.Unmarshal([]byte(`{"limits":{"pings":20,"max_down_bytes":10000000,"max_up_bytes":10000000,"max_seconds":10}}`), &v)
	if v.Limits != (pretest.Limits{Pings: 20, MaxDownBytes: 10_000_000, MaxUpBytes: 10_000_000, MaxSeconds: 10}) {
		t.Errorf("%+v", v.Limits)
	}
}
