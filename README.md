# NVTO Connect

The renter's client for [NVTO](https://nvto.com), a network of dedicated Apple-silicon Macs that are rented whole,
one renter at a time. Connect signs in to your account, lists your leases, and brings a leased Mac's SSH and remote
desktop to ports on your own computer.

NVTO is a remote work machine. It is not a VPN or a proxy, and Connect forwards nothing except SSH and remote
desktop to a machine you are renting.

## Status

Early access. Connect is a command-line program. It builds for macOS, Windows and Linux on x64 and ARM; macOS and
Linux have been run, and **the Windows builds have not been tested on real hardware yet**. The macOS release builds
are signed with a Developer ID and notarized by Apple. The Windows and Linux builds are **not signed**: Windows will
ask before running a downloaded binary, and building from source avoids that.

## Build

Go 1.27 or later.

```sh
git clone https://github.com/nvtohq/connect.git
cd connect
go build -o nvto-connect ./cmd/nvto-connect
```

## Use

```sh
nvto-connect login
nvto-connect leases
nvto-connect open LEASE_ID
```

While `open` runs, the machine is reachable at:

- SSH: `ssh -p 10022 <user>@127.0.0.1`
- Remote desktop: `vnc://127.0.0.1:15900` (macOS Screen Sharing; on Windows and Linux, a VNC viewer you install)

The node serves the machine's screen from outside the machine, and the machine logs its account (`renter`) in by
itself, so the remote desktop opens on the desktop. It asks for its own password, which the node makes for each
machine and leaves in the account; Connect reads it over SSH on every connection, puts it in the link it opens on
macOS (`-open-vnc`, or "Open remote desktop" in the Mac app), and otherwise prints it for you to give your viewer.
The account's own password, which System Settings, installers and the login window ask for, is printed beside it.
Connect prints passwords only to a terminal, never to a log. With an older node, the remote desktop is the machine's
own Screen Sharing, which asks for the account's name and password, and Connect sets that password as before.
Keep Connect up to date: an older Connect gives a current node's remote desktop the wrong password, and changes the
account's password, which stops the machine logging in by itself after a restart.

Before you order, the website can reserve a machine and have Connect test the connection to it:
`nvto-connect test RESERVATION_ID` reaches the reserved machine the way a lease would, measures latency, jitter,
download and upload for at most ten seconds, and prints the platform's rating (smooth, ok or may lag). The Mac app
runs it when the website opens `nvto-connect://test?reservation=RESERVATION_ID`. A test is free and moves at most
what its ticket allows (10 MB each way).

Every command and flag is described in the [Connect reference](https://nvto.com/docs/reference/connect-cli).

## How it connects

Connect joins an encrypted WireGuard-based overlay network for the length of the session. It reaches the machine
directly where the two networks allow it and through an encrypted relay where they do not. The machine admits a
connection only if it presents a lease token signed by the platform; Connect fetches a fresh token for each
connection, tokens last 10 minutes, and access ends when the lease does.

`login` signs in through your browser: it prints a short code and opens app.nvto.com, where you check the
code and approve it. Connect never sees your password, so a counterfeit copy has none to take. Approve only a code
you have just started yourself. This is the only way to sign in. On a computer with no browser, run
`login -no-browser`: it prints the address and the code instead of opening a browser, and you open that address on
any device (your phone, your laptop), sign in there and enter the code. `login` then keeps a session token in your
user configuration directory.

Official builds are published only on this repository's releases page and on https://nvto.com/download. The
macOS builds are signed with NVTO's Developer ID (team ID `7GS265YM4T`) and notarized by Apple; every release has a
`SHA256SUMS` file. See [how to check a download](https://nvto.com/security#software).

The gate, lease token, overlay and tunnel code is shared with the node side and lives in
[nvtohq/node](https://github.com/nvtohq/node).

## Security

Please report vulnerabilities privately: see [SECURITY.md](SECURITY.md).

## Licence

[GNU Affero General Public License v3.0](LICENSE).
