# NodeEx Connect

The renter's client for [NodeEx](https://nodeex.com), a network of dedicated Apple-silicon Macs that are rented whole,
one renter at a time. Connect signs in to your account, lists your leases, and brings a leased Mac's SSH and remote
desktop to ports on your own computer.

NodeEx is a remote work machine. It is not a VPN or a proxy, and Connect forwards nothing except SSH and remote
desktop to a machine you are renting.

## Status

Early access. Connect is a command-line program. It builds for macOS, Windows and Linux on x64 and ARM; macOS and
Linux have been run, and **the Windows builds have not been tested on real hardware yet**. The macOS release builds
are signed with a Developer ID and notarized by Apple. The Windows and Linux builds are **not signed**: Windows will
ask before running a downloaded binary, and building from source avoids that.

## Build

Go 1.27 or later.

```sh
git clone https://github.com/nodeexhq/connect.git
cd connect
go build -o nodeex-connect ./cmd/nodeex-connect
```

## Use

```sh
nodeex-connect login -api https://app.nodeex.com -email you@example.com
nodeex-connect leases
nodeex-connect open LEASE_ID
```

While `open` runs, the machine is reachable at:

- SSH: `ssh -p 10022 <user>@127.0.0.1`
- Remote desktop: `vnc://127.0.0.1:15900` (macOS Screen Sharing; on Windows and Linux, a VNC viewer you install)

Every command and flag is described in the [Connect reference](https://nodeex.com/docs/reference/connect-cli).

## How it connects

Connect joins an encrypted WireGuard-based overlay network for the length of the session. It reaches the machine
directly where the two networks allow it and through an encrypted relay where they do not. The machine admits a
connection only if it presents a lease token signed by the platform; Connect fetches a fresh token for each
connection, tokens last 10 minutes, and access ends when the lease does.

The password is read from the terminal or from `$NODEEX_PASSWORD` and is never stored; `login` keeps a session
token in your user configuration directory.

The gate, lease token, overlay and tunnel code is shared with the node side and lives in
[nodeexhq/miner](https://github.com/nodeexhq/miner).

## Security

Please report vulnerabilities privately: see [SECURITY.md](SECURITY.md).

## Licence

[GNU Affero General Public License v3.0](LICENSE).
