import Foundation

/// One line the core writes with -events.
public struct CoreEvent: Decodable, Equatable {
    public var event: String
    public var userCode: String?
    public var verificationURL: String?
    public var expiresIn: Int?
    public var sshPort: Int?
    public var vncPort: Int?
    public var account: String?
    public var password: String?
    public var sshKey: String?
    public var note: String?
    public var direct: Bool?
    public var relay: String?
    public var latencyMs: Double?
    public var message: String?
    /// A machine check: "ok" or "mismatch", and what did not match.
    public var verdict: String?
    public var problems: [String]?

    enum CodingKeys: String, CodingKey {
        case event, account, password, note, direct, relay, message, verdict, problems
        case userCode = "user_code", verificationURL = "verification_url", expiresIn = "expires_in"
        case sshPort = "ssh_port", vncPort = "vnc_port", sshKey = "ssh_key", latencyMs = "latency_ms"
    }

    public static func parse(_ line: String) -> CoreEvent? {
        guard let data = line.data(using: .utf8) else { return nil }
        return try? JSONDecoder().decode(CoreEvent.self, from: data)
    }
}

/// A lease as `nodeex-connect leases -json` prints it.
public struct Lease: Decodable, Equatable, Identifiable {
    public var id: String
    public var term: String
    public var price: String
    public var status: String
    public var startsAt: Date
    public var endsAt: Date

    enum CodingKeys: String, CodingKey {
        case id, term, price, status
        case startsAt = "starts_at", endsAt = "ends_at"
    }

    public init(id: String, term: String, price: String, status: String, startsAt: Date, endsAt: Date) {
        self.id = id; self.term = term; self.price = price; self.status = status; self.startsAt = startsAt; self.endsAt = endsAt
    }
}

public struct SignInStatus: Decodable, Equatable {
    public var signedIn: Bool
    public var email: String?
    public var unchecked: String?
    enum CodingKeys: String, CodingKey { case signedIn = "signed_in", email, unchecked }
}

/// Runs the Go core that is inside the app.
public struct CoreCLI {
    public let binary: URL
    public let environment: [String: String]

    public init(binary: URL, environment: [String: String] = [:]) {
        self.binary = binary
        self.environment = environment
    }

    /// Runs a command to completion and returns its standard output.
    public func run(_ args: [String]) throws -> Data {
        let p = Process()
        p.executableURL = binary
        p.arguments = args
        p.environment = ProcessInfo.processInfo.environment.merging(environment) { _, new in new }
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        try p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        let errData = err.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            let msg = String(data: errData, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            throw CoreError.failed(msg.replacingOccurrences(of: "nodeex-connect: ", with: ""))
        }
        return data
    }

    public func status() throws -> SignInStatus {
        try JSONDecoder().decode(SignInStatus.self, from: run(["status", "-json"]))
    }

    public func leases() throws -> [Lease] {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let s = try decoder.singleValueContainer().decode(String.self)
            if let date = CoreCLI.date(s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "bad date \(s)"))
        }
        return try d.decode([Lease].self, from: run(["leases", "-json"]))
    }

    public func logout() throws {
        _ = try run(["logout"])
    }

    static func date(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }
}

public enum CoreError: Error, CustomStringConvertible {
    case failed(String)
    public var description: String {
        switch self {
        case .failed(let m): return m.isEmpty ? "The Connect core failed." : m
        }
    }
}

/// A long-running core command whose events arrive as they happen, on the
/// main queue.
public final class CoreProcess {
    private let process = Process()
    private var buffer = Data()
    private var lastError: String?
    public var onEvent: ((CoreEvent) -> Void)?
    /// Called once when the process ends, with its last error if it failed.
    public var onExit: ((String?) -> Void)?

    public init(cli: CoreCLI, args: [String]) {
        process.executableURL = cli.binary
        process.arguments = args
        process.environment = ProcessInfo.processInfo.environment.merging(cli.environment) { _, new in new }
    }

    public func start() throws {
        let out = Pipe()
        process.standardOutput = out
        process.standardError = FileHandle.nullDevice
        out.fileHandleForReading.readabilityHandler = { [weak self] h in
            let chunk = h.availableData
            DispatchQueue.main.async { self?.consume(chunk) }
        }
        process.terminationHandler = { [weak self] p in
            DispatchQueue.main.async {
                out.fileHandleForReading.readabilityHandler = nil
                guard let self else { return }
                self.consume(out.fileHandleForReading.readDataToEndOfFile())
                let failed = p.terminationStatus != 0 && p.terminationReason == .exit
                self.onExit?(failed ? (self.lastError ?? "The Connect core stopped.") : nil)
                self.onExit = nil
            }
        }
        try process.run()
    }

    /// Asks the core to stop; it disconnects cleanly on SIGTERM.
    public func stop() {
        if process.isRunning { process.terminate() }
    }

    public var isRunning: Bool { process.isRunning }

    private func consume(_ chunk: Data) {
        guard !chunk.isEmpty else { return }
        buffer.append(chunk)
        while let nl = buffer.firstIndex(of: 0x0A) {
            let line = buffer[buffer.startIndex..<nl]
            buffer.removeSubrange(buffer.startIndex...nl)
            if let s = String(data: line, encoding: .utf8), let e = CoreEvent.parse(s) {
                if e.event == "error" { lastError = e.message }
                onEvent?(e)
            }
        }
    }
}
