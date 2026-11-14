package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// openBrowser shows url in the user's browser. It only opens web addresses,
// so a reply from the server cannot make it start anything else.
func openBrowser(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("not a web address: %q", raw)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u.String())
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u.String())
	default:
		cmd = exec.Command("xdg-open", u.String())
	}
	return cmd.Start()
}

// browserLogin signs in the way GitHub's and the cloud providers' command-line
// tools do: this program shows a short code and opens the NodeEx site; the
// user, signed in there, checks the code and approves. No password is ever
// typed into this program, so a counterfeit copy has none to take.
func browserLogin(ctx context.Context, api string, noBrowser bool) (string, error) {
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := call(ctx, session{API: api}, "POST", "/v1/auth/device",
		map[string]string{"client": "nodeex-connect " + version}, &start); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "\nTo sign in, open this page and approve the code:\n\n    %s\n\n    code: %s\n\n", start.VerificationURL, start.UserCode)
	fmt.Fprintln(os.Stderr, "Approve only if the page is on nodeex.com and shows this same code.")
	if !noBrowser {
		if err := openBrowser(start.VerificationURL); err != nil {
			fmt.Fprintln(os.Stderr, "(could not open a browser; open the address yourself)")
		}
	}
	fmt.Fprintln(os.Stderr, "waiting for approval…")

	interval := time.Duration(max(start.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(start.ExpiresIn, 60)) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		var res struct {
			Token string `json:"token"`
		}
		err := call(ctx, session{API: api}, "POST", "/v1/auth/device/token",
			map[string]string{"device_code": start.DeviceCode}, &res)
		var ae *apiError
		switch {
		case err == nil:
			return res.Token, nil
		case errors.As(err, &ae) && ae.Code == "authorization_pending":
			continue
		case errors.As(err, &ae) && ae.Code == "access_denied":
			return "", errors.New("the sign-in was denied on the website")
		case errors.As(err, &ae) && ae.Code == "expired_token":
			return "", errors.New("the code expired; run `nodeex-connect login` again")
		case errors.As(err, &ae):
			return "", err
		default:
			// A network hiccup: keep waiting until the code expires.
			continue
		}
	}
	return "", errors.New("the code expired; run `nodeex-connect login` again")
}
