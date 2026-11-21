import Foundation

/// The result of a connection test before an order, as the core reports it
/// in its test_result event, put in plain words.
public struct TestOutcome: Equatable {
    public var verdict: String
    public var latencyMs: Double
    public var jitterMs: Double
    public var downMbps: Double
    public var upMbps: Double
    public var direct: Bool
    public var relay: String?

    public init?(_ e: CoreEvent) {
        guard e.event == "test_result", let verdict = e.verdict, let latency = e.latencyMs, let jitter = e.jitterMs,
              let down = e.downMbps, let up = e.upMbps else { return nil }
        self.verdict = verdict
        self.latencyMs = latency
        self.jitterMs = jitter
        self.downMbps = down
        self.upMbps = up
        self.direct = e.direct ?? false
        self.relay = e.relay.flatMap { $0.isEmpty ? nil : $0 }
    }

    /// The verdict in a few words.
    public var headline: String {
        switch verdict {
        case "smooth": return "Smooth"
        case "ok": return "Usable"
        case "may_lag": return "May lag"
        default: return verdict
        }
    }

    /// What the verdict means for a remote desktop.
    public var explanation: String {
        switch verdict {
        case "smooth":
            return "Remote desktop should feel responsive on this connection."
        case "ok":
            return "Remote desktop will work, though it may not always feel instant."
        case "may_lag":
            return direct
                ? "Remote desktop may stutter on this connection."
                : "The connection goes through a relay, so remote desktop may stutter."
        default:
            return ""
        }
    }

    public var path: String {
        direct ? "Direct" : "Relayed" + (relay.map { " via \($0)" } ?? "")
    }

    public var latency: String { String(format: "%.0f ms", latencyMs) }
    public var jitter: String { String(format: "%.0f ms", jitterMs) }
    public var down: String { TestOutcome.mbps(downMbps) }
    public var up: String { TestOutcome.mbps(upMbps) }

    static func mbps(_ v: Double) -> String {
        String(format: v < 10 ? "%.1f Mbit/s" : "%.0f Mbit/s", v)
    }
}
