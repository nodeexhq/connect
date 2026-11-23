import Foundation

/// What a nvto-connect:// link asks for. The NVTO console links to
/// nvto-connect://open?lease=<id>&mode=desktop (or ssh), and the order page
/// to nvto-connect://test?reservation=<id> for the connection test before
/// an order; any web page could open such a link, so it can only ever reach
/// the signed-in user's own leases and reservations, and only the things
/// below.
public enum LinkRequest: Equatable {
    case open(lease: String, mode: OpenMode)
    case test(reservation: String)
    case signIn

    public init?(_ url: URL) {
        guard url.scheme?.lowercased() == "nvto-connect",
              let parts = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return nil }
        let action = (parts.host ?? "").lowercased()
        let query = Dictionary((parts.queryItems ?? []).map { ($0.name, $0.value ?? "") }, uniquingKeysWith: { a, _ in a })
        switch action {
        case "open":
            guard let lease = query["lease"], LinkRequest.isLeaseID(lease),
                  let mode = OpenMode(rawValue: (query["mode"] ?? "desktop").lowercased()) else { return nil }
            self = .open(lease: lease.lowercased(), mode: mode)
        case "test":
            guard let reservation = query["reservation"], LinkRequest.isLeaseID(reservation) else { return nil }
            self = .test(reservation: reservation.lowercased())
        case "login", "sign-in":
            self = .signIn
        default:
            return nil
        }
    }

    /// Lease and reservation identifiers are UUIDs; nothing else is passed
    /// to the core.
    public static func isLeaseID(_ s: String) -> Bool {
        UUID(uuidString: s) != nil
    }
}

public enum OpenMode: String, Equatable {
    case desktop
    case ssh
}
