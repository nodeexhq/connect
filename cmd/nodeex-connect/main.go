// nodeex-connect is the renter-side client. It signs in, lists leases, and
// opens a leased machine's SSH and remote desktop on local ports.
//
//	nodeex-connect login [-no-browser]       (approve in the browser)
//	nodeex-connect login -email EMAIL        (password, for machines without a browser)
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
	case "logout":
		err = cmdLogout(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
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
		emit("error", map[string]any{"message": err.Error()})
		fmt.Fprintln(os.Stderr, "nodeex-connect:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: nodeex-connect <login|logout|status|leases|open|version> [flags]")
	os.Exit(2)
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	api := fs.String("api", envOr("NODEEX_API", defaultAPI), "platform API URL (or $NODEEX_API)")
	email := fs.String("email", "", "sign in with email and password instead of the browser")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in address instead of opening a browser")
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.BoolVar(&eventsOn, "events", false, "write progress as JSON lines on standard output, for the app")
	fs.Parse(args)
	if *api == "" {
		return errors.New("usage: nodeex-connect login [-no-browser] [-email YOU@EXAMPLE.COM]")
	}
	var token string
	if *email == "" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		t, err := browserLogin(ctx, *api, *noBrowser)
		if err != nil {
			return err
		}
		token = t
	} else {
		t, err := passwordLogin(*api, *email)
		if err != nil {
			return err
		}
		token = t
	}
	if err := saveSession(*dir, session{API: *api, Token: token}); err != nil {
		return err
	}
	emit("signed_in", nil)
	return nil
}

// passwordLogin is for a computer with no browser at hand. The password
// comes from the terminal or $NODEEX_PASSWORD, never a flag, so it stays
// out of shell history and process listings.
func passwordLogin(api, email string) (string, error) {
	password := os.Getenv("NODEEX_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		password = string(b)
	}
	var res struct {
		Token string `json:"token"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := call(ctx, session{API: api}, "POST", "/v1/auth/login",
		map[string]string{"email": email, "password": password}, &res)
	return res.Token, err
}

func saveSession(dir string, s session) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(sessionPath(dir), b, 0o600); err != nil {
		return err
	}
	if !eventsOn {
		fmt.Println("signed in")
	}
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
	asJSON := fs.Bool("json", false, "print the leases as JSON")
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
	if *asJSON {
		if res.Leases == nil {
			res.Leases = []lease{}
		}
		return json.NewEncoder(os.Stdout).Encode(res.Leases)
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
	fs.BoolVar(&eventsOn, "events", false, "write progress as JSON lines on standard output, for the app")
	noCheck := fs.Bool("no-check", false, "do not check the machine against what was ordered")
	noSSHConfig := fs.Bool("no-ssh-config", false, "do not name the machine in ~/.ssh/config while connected")
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
		var ae *apiError
		switch {
		case errors.As(err, &ae) && ae.Code == "not_found":
			return errors.New("this lease is not in the signed-in account")
		case errors.As(err, &ae) && ae.Status == http.StatusUnauthorized:
			return errors.New("not signed in, or the session has expired; sign in again")
		}
		return err
	}
	// Register this computer's key for the lease, so the machine lets it
	// in. A node that does not give each lease its own machine ignores it.
	registered := false
	if pub, err := ensureSSHKey(*dir); err != nil {
		log.Printf("could not prepare an SSH key: %v", err)
	} else if err := call(ctx, s, "PUT", "/v1/leases/"+leaseID+"/ssh-keys", map[string]any{"keys": []string{pub}}, nil); err != nil {
		log.Printf("could not register this computer's SSH key: %v", err)
	} else {
		registered = true
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
					emit("lease_ended", nil)
					forgetSSHHost(leaseID)
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
	// forward listens on a local port, 0 for any free one, and returns the
	// port it got.
	forward := func(port int, service string) (int, error) {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return 0, fmt.Errorf("local port %d: %w", port, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tunnel.Serve(ctx, ln, dialService(service), log.Printf)
		}()
		return ln.Addr().(*net.TCPAddr).Port, nil
	}
	if *sshPort, err = forward(*sshPort, leasetoken.ServiceSSH); err != nil {
		return err
	}
	if *vncPort, err = forward(*vncPort, leasetoken.ServiceVNC); err != nil {
		return err
	}
	// Set once the lease's private folder has been unlocked, so that it is
	// unlocked again if the machine restarts while connected.
	privateReady := false
	emit("listening", map[string]any{"ssh_port": *sshPort, "vnc_port": *vncPort})
	log.Printf("ready; leave this running while you use the machine, and press Ctrl+C to disconnect")
	if !registered {
		emit("ready", map[string]any{"ssh_port": *sshPort, "vnc_port": *vncPort})
		log.Printf("  ssh:             ssh -p %d <user>@127.0.0.1", *sshPort)
		log.Printf("  remote desktop:  %s", desktopHint("127.0.0.1", *vncPort))
	} else {
		// The node installs the key with its next heartbeat, so the machine
		// takes a moment to let it in.
		log.Printf("  waiting for the machine to accept this computer's key")
		password, err := prepareMachine(ctx, *dir, leaseID, *sshPort, 90*time.Second)
		ready := map[string]any{"ssh_port": *sshPort, "vnc_port": *vncPort, "ssh_key": sshKeyPath(*dir)}
		private, privateErr := "", error(nil)
		if err == nil {
			ready["account"], ready["password"] = machineAccount, password
			// A name in ~/.ssh for terminals and editors (VS Code, Cursor,
			// JetBrains Gateway). Best effort: without it, the port works.
			if !*noSSHConfig {
				if host, err := writeSSHHost(*dir, leaseID, *sshPort); err == nil {
					ready["ssh_host"] = host
					defer removeSSHHost(leaseID)
				} else {
					log.Printf("  could not add the machine to ~/.ssh/config: %v", err)
				}
			}
			if private, privateErr = preparePrivateVolume(ctx, *dir, leaseID, *sshPort); privateErr == nil {
				ready["private"] = private
				privateReady = true
			} else if ctx.Err() == nil {
				ready["private_note"] = privateErr.Error()
			}
		} else if !errors.Is(err, errNoMachine) && ctx.Err() == nil {
			ready["account"] = machineAccount
			ready["note"] = err.Error()
		}
		if ctx.Err() == nil {
			emit("ready", ready)
		}
		if err == nil && !*noCheck {
			// In the background: the machine is usable meanwhile.
			wg.Add(1)
			go func() {
				defer wg.Done()
				checkLease(ctx, s, *dir, leaseID, *sshPort)
			}()
		}
		switch {
		case err == nil:
			if host, ok := ready["ssh_host"].(string); ok {
				log.Printf("  ssh:             ssh %s", host)
				log.Printf("  editors:         VS Code or Cursor Remote-SSH, JetBrains Gateway: connect to host %s", host)
			} else {
				log.Printf("  ssh:             ssh -i %s -p %d %s@127.0.0.1", sshKeyPath(*dir), *sshPort, machineAccount)
			}
			log.Printf("  remote desktop:  %s", desktopHint("127.0.0.1", *vncPort))
			log.Printf("                   user %s, password %s", machineAccount, password)
			if privateErr == nil {
				log.Printf("  private folder:  %s, an encrypted volume only this computer can unlock; it is locked whenever the machine stops", private)
			} else {
				log.Printf("  private folder:  not available (%v)", privateErr)
			}
			log.Printf("  The machine is yours for this lease, with sudo, and is erased when the lease ends.")
		case errors.Is(err, errNoMachine):
			log.Printf("  This node did not accept the key: it has not given the lease a machine of its own.")
			log.Printf("  ssh:             ssh -p %d <user>@127.0.0.1", *sshPort)
			log.Printf("  remote desktop:  %s", desktopHint("127.0.0.1", *vncPort))
		case ctx.Err() != nil:
			return nil
		default:
			log.Printf("  ssh:             ssh -i %s -p %d %s@127.0.0.1", sshKeyPath(*dir), *sshPort, machineAccount)
			log.Printf("  remote desktop:  %s (no password could be set: %v)", desktopHint("127.0.0.1", *vncPort), err)
		}
	}
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
				emit("path", map[string]any{"direct": path.Direct, "relay": path.Relay, "latency_ms": path.Latency * 1000})
				// Tell the platform how the connection is going: direct or
				// relayed, and the latency. Nothing about what is sent
				// over it. A failed report is not worth interrupting for.
				call(pctx, s, "POST", "/v1/leases/"+leaseID+"/session", map[string]any{
					"direct": path.Direct, "relay": path.Relay, "latency_ms": path.Latency * 1000,
				}, nil)
			}
			// A machine that restarted comes back with the private folder
			// locked, and only this computer has its passphrase.
			if privateReady {
				uctx, ucancel := context.WithTimeout(ctx, 20*time.Second)
				if again, err := reopenPrivateVolume(uctx, *dir, leaseID, *sshPort); again && err == nil {
					log.Printf("the machine restarted; its private folder is unlocked again")
					emit("private_unlocked", nil)
				} else if again && uctx.Err() == nil {
					log.Printf("the machine restarted, and its private folder could not be unlocked: %v", err)
				}
				ucancel()
			}
			// A lease that has ended has nothing left to connect to: say
			// so and stop, instead of holding dead ports open for good.
			// Only the platform's own word counts; a failed request does not.
			if ended, known := leaseEnded(pctx, s, leaseID); known && ended {
				pcancel()
				log.Printf("lease %s has ended; disconnecting", leaseID)
				emit("lease_ended", nil)
				forgetSSHHost(leaseID)
				cancel()
				return
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
	emit("closed", nil)
	return nil
}

// leaseEnded asks the platform whether the lease is over. known is false
// when the platform could not be asked or does not list the lease.
func leaseEnded(ctx context.Context, s session, leaseID string) (ended, known bool) {
	var res struct {
		Leases []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"leases"`
	}
	if err := call(ctx, s, "GET", "/v1/leases", nil, &res); err != nil {
		return false, false
	}
	for _, l := range res.Leases {
		if l.ID == leaseID {
			return l.Status == "ended", l.Status != ""
		}
	}
	return false, false
}

// cmdLogout ends the session on the platform and forgets it here.
func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.Parse(args)
	s, err := loadSession(*dir)
	if err != nil {
		return nil // not signed in: nothing to do
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// A session the platform has already forgotten is no reason to keep
	// the file.
	call(ctx, s, "POST", "/v1/auth/logout", nil, nil)
	if err := os.Remove(sessionPath(*dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("signed out")
	return nil
}

// cmdStatus says whether this computer is signed in, and to which account,
// asking the platform so that an expired session shows as signed out.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	asJSON := fs.Bool("json", false, "print the status as JSON")
	fs.Parse(args)
	out := map[string]any{"signed_in": false}
	if s, err := loadSession(*dir); err == nil {
		var me struct {
			Email string `json:"email"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := call(ctx, s, "GET", "/v1/me", nil, &me)
		var ae *apiError
		switch {
		case err == nil:
			out["signed_in"], out["email"] = true, me.Email
		case errors.As(err, &ae) && ae.Status == http.StatusUnauthorized:
		default:
			// Offline: the session is there, but could not be checked.
			out["signed_in"], out["unchecked"] = true, err.Error()
		}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	switch {
	case out["signed_in"] == true && out["email"] != nil:
		fmt.Printf("signed in as %v\n", out["email"])
	case out["signed_in"] == true:
		fmt.Printf("signed in (could not reach the platform to check: %v)\n", out["unchecked"])
	default:
		fmt.Println("not signed in")
	}
	return nil
}
