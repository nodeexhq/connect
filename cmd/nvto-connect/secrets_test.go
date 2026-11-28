package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseNodeSecrets(t *testing.T) {
	d, a := parseNodeSecrets(".nvto/desktop-password Ab3dEf7h\n.nvto/account-password abcde-fghjk-mnpqr-stuvw\n")
	if d != "Ab3dEf7h" || a != "abcde-fghjk-mnpqr-stuvw" {
		t.Fatalf("got %q %q", d, a)
	}
	// Not the eight characters VNC uses, a space, a control character, an
	// empty value, an unknown file, a short account password: all ignored.
	for _, out := range []string{
		".nvto/desktop-password Ab3dEf7",
		".nvto/desktop-password Ab3dEf7hX",
		".nvto/desktop-password Ab3 Ef7h",
		".nvto/desktop-password Ab3\x1bEf7h",
		".nvto/desktop-password ",
		".nvto/other Ab3dEf7h",
		".nvto/account-password short",
		"",
	} {
		if d, a := parseNodeSecrets(out); d != "" || a != "" {
			t.Errorf("%q: got %q %q", out, d, a)
		}
	}
	// Windows line ends are tolerated.
	if d, _ := parseNodeSecrets(".nvto/desktop-password Ab3dEf7h\r\n"); d != "Ab3dEf7h" {
		t.Errorf("CRLF: %q", d)
	}
}

// The machine's passwords over a real SSH connection to a shell on this
// computer, whose home stands for the account's. Setting the account's
// password is replaced by writing it to a file in that home.
func TestMachineSecretsOver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	saved := setAccountPassword
	setAccountPassword = `read -r p && printf '%s' "$p" > "$HOME/set-password"`
	defer func() { setAccountPassword = saved }()

	write := func(t *testing.T, home, name, text string) {
		t.Helper()
		os.MkdirAll(filepath.Join(home, ".nvto"), 0o700)
		if err := os.WriteFile(filepath.Join(home, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("node serves the desktop", func(t *testing.T) {
		home, dir := t.TempDir(), t.TempDir()
		write(t, home, desktopPasswordFile, "Ab3dEf7h\n")
		write(t, home, accountPasswordFile, "abcde-fghjk-mnpqr-stuvw\n")
		s, err := machineSecretsOver(shellSSHServerIn(t, home), dir, "L1")
		if err != nil {
			t.Fatal(err)
		}
		if s.DesktopPassword != "Ab3dEf7h" || s.AccountPassword != "abcde-fghjk-mnpqr-stuvw" {
			t.Fatalf("got %+v", s)
		}
		// Nothing changed in the machine, nothing kept here.
		if _, err := os.Stat(filepath.Join(home, "set-password")); err == nil {
			t.Error("the account's password was changed")
		}
		if _, err := os.Stat(accessPath(dir, "L1")); err == nil {
			t.Error("the node's password was saved on this computer")
		}
	})

	t.Run("older node", func(t *testing.T) {
		home, dir := t.TempDir(), t.TempDir()
		c := shellSSHServerIn(t, home)
		s, err := machineSecretsOver(c, dir, "L2")
		if err != nil {
			t.Fatal(err)
		}
		set, _ := os.ReadFile(filepath.Join(home, "set-password"))
		if s.DesktopPassword != "" || s.AccountPassword == "" || string(set) != s.AccountPassword {
			t.Fatalf("got %+v, set %q", s, set)
		}
		if loadAccess(dir, "L2").DesktopPassword != s.AccountPassword {
			t.Error("the password Connect chose was not kept")
		}
		// The next connection uses the kept password and sets nothing.
		os.Remove(filepath.Join(home, "set-password"))
		again, err := machineSecretsOver(c, dir, "L2")
		if err != nil || again != s {
			t.Fatalf("again: %+v %v", again, err)
		}
		if _, err := os.Stat(filepath.Join(home, "set-password")); err == nil {
			t.Error("the password was set again")
		}
	})

	t.Run("account an older node made, desktop a newer one serves", func(t *testing.T) {
		home, dir := t.TempDir(), t.TempDir()
		write(t, home, desktopPasswordFile, "Ab3dEf7h\n")
		saveAccess(dir, "L3", machineAccess{DesktopPassword: "kept-earlier"})
		s, err := machineSecretsOver(shellSSHServerIn(t, home), dir, "L3")
		if err != nil {
			t.Fatal(err)
		}
		if s.DesktopPassword != "Ab3dEf7h" || s.AccountPassword != "kept-earlier" {
			t.Fatalf("got %+v", s)
		}
	})

	t.Run("a desktop password of the wrong length is not used", func(t *testing.T) {
		home, dir := t.TempDir(), t.TempDir()
		write(t, home, desktopPasswordFile, "toolongforvnc\n")
		write(t, home, accountPasswordFile, "abcde-fghjk-mnpqr-stuvw\n")
		s, err := machineSecretsOver(shellSSHServerIn(t, home), dir, "L4")
		if err != nil || s.DesktopPassword != "" || s.AccountPassword != "abcde-fghjk-mnpqr-stuvw" {
			t.Fatalf("got %+v %v", s, err)
		}
	})
}

// The node's files are read by the account's shell, which is zsh in the
// machine: the command must also work there, files or none.
func TestReadNodeSecretsInZsh(t *testing.T) {
	if _, err := os.Stat("/bin/zsh"); err != nil {
		t.Skip("no zsh")
	}
	home := t.TempDir()
	run := func() string {
		t.Helper()
		cmd := exec.Command("/bin/zsh", "-c", readNodeSecrets)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return string(out)
	}
	if out := run(); strings.TrimSpace(out) != "" {
		t.Fatalf("no files: %q", out)
	}
	os.MkdirAll(filepath.Join(home, ".nvto"), 0o700)
	os.WriteFile(filepath.Join(home, desktopPasswordFile), []byte("Ab3dEf7h\n"), 0o600)
	if d, a := parseNodeSecrets(run()); d != "Ab3dEf7h" || a != "" {
		t.Fatalf("got %q %q", d, a)
	}
}
