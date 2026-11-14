import Foundation

/// What a nodeex-connect:// link asks for. The NodeEx console links to
/// nodeex-connect://open?lease=<id>&mode=desktop (or ssh); any web page
/// could open such a link, so it can only ever reach the signed-in user's
/// own leases, and only the two things below.
public enum LinkRequest: Equatable {
    case open(lease: String, mode: OpenMode)
    case signIn

    public init?(_ url: URL) {
        guard url.scheme?.lowercased() == "nodeex-connect",
              let parts = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return nil }
        let action = (parts.host ?? "").lowercased()
        let query = Dictionary((parts.queryItems ?? []).map { ($0.name, $0.value ?? "") }, uniquingKeysWith: { a, _ in a })
        switch action {
        case "open":
            guard let lease = query["lease"], LinkRequest.isLeaseID(lease),
                  let mode = OpenMode(rawValue: (query["mode"] ?? "desktop").lowercased()) else { return nil }
            self = .open(lease: lease.lowercased(), mode: mode)
        case "login", "sign-in":
            self = .signIn
        default:
            return nil
        }
    }

    /// Lease identifiers are UUIDs; nothing else is passed to the core.
    public static func isLeaseID(_ s: String) -> Bool {
        UUID(uuidString: s) != nil
    }
}

public enum OpenMode: String, Equatable {
    case desktop
    case ssh
}
