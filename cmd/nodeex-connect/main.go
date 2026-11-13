// nodeex-connect is the renter-side client. It signs in, lists leases, and
// opens a leased machine's SSH and remote desktop on local ports.
//
//	nodeex-connect login -api URL -email EMAIL
//	nodeex-connect leases
//	nodeex-connect open LEASE_ID [-ssh-port 10022] [-vnc-port 15900] [-open-vnc]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/nodeexhq/miner/gate"
	"github.com/nodeexhq/miner/leasetoken"
	"github.com/nodeexhq/miner/overlay"
	"github.com/nodeexhq/miner/tunnel"
	"golang.org/x/term"
)

type session struct {
	API   string `json:"api"`
	Token string `json:"token"`
}

// defaultAPI is the platform's own address, so renters need not type it.
const defaultAPI = "https://app.nodeex.com"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// defaultDir is where the session is kept: the user's own configuration
// directory on each system (~/Library/Application Support, %AppData%,
// ~/.config). On Windows that directory is private to the user by its
// permissions; the Unix mode bits set below have no effect there.
func defaultDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "NodeEx", "connect")
	}
	return "nodeex-connect"
}

func sessionPath(dir string) string { return filepath.Join(dir, "session.json") }

func loadSession(dir string) (session, error) {
	var s session
	b, err := os.ReadFile(sessionPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return s, errors.New("not signed in; run `nodeex-connect login` first")
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// apiError is an error response from the platform.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (%s)", e.Message, e.Code) }

func call(ctx context.Context, s session, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.API+path, body)
	if err != nil {
		return err
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error.Code == "" {
			e.Error.Code, e.Error.Message = "http_error", fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return &apiError{res.StatusCode, e.Error.Code, e.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "login":
		err = cmdLogin(os.Args[2:])
	case "leases":
		err = cmdLeases(os.Args[2:])
	case "open":
		err = cmdOpen(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("nodeex-connect %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodeex-connect:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: nodeex-connect <login|leases|open|version> [flags]")
	os.Exit(2)
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	api := fs.String("api", envOr("NODEEX_API", defaultAPI), "platform API URL (or $NODEEX_API)")
	email := fs.String("email", "", "account email")
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.Parse(args)
	if *api == "" || *email == "" {
		return errors.New("usage: nodeex-connect login -email YOU@EXAMPLE.COM")
	}
	// The password comes from the terminal or $NODEEX_PASSWORD, never a
	// flag, so it stays out of shell history and process listings.
	password := os.Getenv("NODEEX_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		password = string(b)
	}
	var res struct {
		Token string `json:"token"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := call(ctx, session{API: *api}, "POST", "/v1/auth/login",
		map[string]string{"email": *email, "password": password}, &res)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(session{API: *api, Token: res.Token}, "", "  ")
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(sessionPath(*dir), b, 0o600); err != nil {
		return err
	}
	fmt.Println("signed in")
	return nil
}

type lease struct {
	ID       string    `json:"id"`
	Term     string    `json:"term"`
	Price    string    `json:"price"`
	Status   string    `json:"status"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

func cmdLeases(args []string) error {
	fs := flag.NewFlagSet("leases", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.Parse(args)
	s, err := loadSession(*dir)
	if err != nil {
		return err
	}
	var res struct {
		Leases []lease `json:"leases"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := call(ctx, s, "GET", "/v1/leases", nil, &res); err != nil {
		return err
	}
	if len(res.Leases) == 0 {
		fmt.Println("no leases")
		return nil
	}
	for _, l := range res.Leases {
		fmt.Printf("%s  %-6s  %-5s  %8s  ends %s\n", l.ID, l.Status, l.Term, l.Price, l.EndsAt.Local().Format(time.DateTime))
	}
	return nil
}

// tokens hands out a valid lease token, fetching a new one from the platform
// shortly before the cached one expires.
type tokens struct {
	s     session
	lease string

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (t *tokens) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Until(t.expires) > time.Minute {
		return t.token, nil
	}
	var res struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := call(ctx, t.s, "POST", "/v1/leases/"+t.lease+"/connect-token", nil, &res); err != nil {
		return "", err
	}
	t.token, t.expires = res.Token, res.ExpiresAt
	return t.token, nil
}

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	sshPort := fs.Int("ssh-port", 10022, "local port for SSH")
	vncPort := fs.Int("vnc-port", 15900, "local port for remote desktop")
	openVNC := fs.Bool("open-vnc", false, "open the remote desktop viewer once connected")
	// Accept the lease ID before or after the flags.
	var leaseID string
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		leaseID, args = args[0], args[1:]
	}
	fs.Parse(args)
	if leaseID == "" {
		leaseID = fs.Arg(0)
	}
	if leaseID == "" {
		return errors.New("usage: nodeex-connect open LEASE_ID")
	}
	s, err := loadSession(*dir)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var key struct {
		ControlURL string `json:"control_url"`
		AuthKey    string `json:"auth_key"`
		NodeAddr   string `json:"node_addr"`
		GatePort   int    `json:"gate_port"`
	}
	if err := call(ctx, s, "POST", "/v1/leases/"+leaseID+"/overlay-key", nil, &key); err != nil {
		return err
	}
	node, err := netip.ParseAddr(key.NodeAddr)
	if err != nil {
		return errors.New("platform returned an invalid machine address")
	}
	gateAddr := netip.AddrPortFrom(node, uint16(key.GatePort)).String()

	// Each run joins as a fresh ephemeral client, so the overlay state is
	// throwaway.
	stateDir, err := os.MkdirTemp("", "nodeex-connect-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)
	srv, err := overlay.Start(ctx, overlay.Config{
		ControlURL: key.ControlURL, AuthKey: key.AuthKey,
		Hostname: "c-" + leaseID, StateDir: stateDir,
	}, log.Printf)
	if err != nil {
		return err
	}
	defer srv.Close()

	tok := &tokens{s: s, lease: leaseID}
	if _, err := tok.get(ctx); err != nil {
		return err
	}

	// dialService opens a gated connection to one service on the machine.
	dialService := func(service string) tunnel.DialFunc {
		return func(ctx context.Context) (net.Conn, error) {
			token, err := tok.get(ctx)
			if err != nil {
				var ae *apiError
				if errors.As(err, &ae) && ae.Code == "lease_not_active" {
					log.Printf("the lease has ended; closing")
					cancel()
				}
				return nil, err
			}
			conn, err := srv.Dial(ctx, "tcp", gateAddr)
			if err != nil {
				return nil, err
			}
			if err := gate.Dial(conn, service, token); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		}
	}

	var wg sync.WaitGroup
	forward := func(port int, service string) error {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return fmt.Errorf("local port %d: %w", port, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tunnel.Serve(ctx, ln, dialService(service), log.Printf)
		}()
		return nil
	}
	if err := forward(*sshPort, leasetoken.ServiceSSH); err != nil {
		return err
	}
	if err := forward(*vncPort, leasetoken.ServiceVNC); err != nil {
		return err
	}
	log.Printf("ready; leave this running while you use the machine, and press Ctrl+C to disconnect")
	log.Printf("  ssh:             ssh -p %d <user>@127.0.0.1", *sshPort)
	log.Printf("  remote desktop:  %s", desktopHint("127.0.0.1", *vncPort))
	if *openVNC {
		if err := openDesktop(ctx, "127.0.0.1", *vncPort); err != nil {
			log.Printf("could not open the remote desktop: %v", err)
		}
	}

	// Report the path to the machine periodically.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			if path, err := overlay.Probe(pctx, srv, node); err == nil {
				log.Printf("path: %s", path)
				// Tell the platform how the connection is going: direct or
				// relayed, and the latency. Nothing about what is sent
				// over it. A failed report is not worth interrupting for.
				call(pctx, s, "POST", "/v1/leases/"+leaseID+"/session", map[string]any{
					"direct": path.Direct, "relay": path.Relay, "latency_ms": path.Latency * 1000,
				}, nil)
			}
			pcancel()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	wg.Wait()
	return nil
}
