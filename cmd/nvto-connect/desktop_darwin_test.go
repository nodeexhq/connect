package main

import (
	"net/url"
	"testing"
)

// Screen Sharing signs in to the node's desktop with the password in the
// link; the link names no account, as that server asks for none.
func TestDesktopURL(t *testing.T) {
	if got := desktopURL("127.0.0.1", 50059, ""); got != "vnc://127.0.0.1:50059" {
		t.Errorf("without a password: %s", got)
	}
	got := desktopURL("127.0.0.1", 50059, "Ab3dEf7h")
	if got != "vnc://:Ab3dEf7h@127.0.0.1:50059" {
		t.Errorf("with a password: %s", got)
	}
	u, err := url.Parse(desktopURL("127.0.0.1", 1, "a/b@c:d"))
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := u.User.Password(); p != "a/b@c:d" || u.User.Username() != "" || u.Port() != "1" {
		t.Errorf("escaped: %s", u)
	}
}
