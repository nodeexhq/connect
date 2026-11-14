import Foundation

/// Hands a connection to the apps macOS already has: Screen Sharing for the
/// desktop, Terminal for SSH.
public enum Handoff {
    /// A vnc:// address for Screen Sharing. With the account and password
    /// in it, Screen Sharing signs in without asking.
    public static func desktopURL(port: Int, account: String?, password: String?) -> URL {
        var c = URLComponents()
        c.scheme = "vnc"
        c.host = "127.0.0.1"
        c.port = port
        if let account, let password, !account.isEmpty, !password.isEmpty {
            c.user = account
            c.password = password
        }
        return c.url!
    }

    /// Writes a .command file that Terminal runs to open SSH to the lease's
    /// machine with this computer's key. Each lease keeps its own known-hosts
    /// file: every lease is a new machine behind the same local address.
    public static func sshCommandFile(directory: URL, lease: String, port: Int, account: String?, key: String?) throws -> URL {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let known = directory.appendingPathComponent("known_hosts-\(lease)").path
        var args = ["/usr/bin/ssh", "-p", String(port),
                    "-o", "StrictHostKeyChecking=accept-new",
                    "-o", "UserKnownHostsFile=\(known)"]
        if let key, !key.isEmpty { args += ["-i", key, "-o", "IdentitiesOnly=yes"] }
        args.append("\(account ?? "renter")@127.0.0.1")
        let script = "#!/bin/sh\nclear\nexec " + args.map(shellQuote).joined(separator: " ") + "\n"
        let file = directory.appendingPathComponent("ssh-\(lease).command")
        try script.write(to: file, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
        return file
    }

    static func shellQuote(_ s: String) -> String {
        if s.range(of: "^[A-Za-z0-9@%+=:,./_-]+$", options: .regularExpression) != nil { return s }
        return "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}
