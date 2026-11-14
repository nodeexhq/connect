import XCTest
@testable import ConnectKit

final class ConnectKitTests: XCTestCase {
    func testLinks() {
        let id = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
        XCTAssertEqual(LinkRequest(URL(string: "nodeex-connect://open?lease=\(id)&mode=ssh")!), .open(lease: id, mode: .ssh))
        XCTAssertEqual(LinkRequest(URL(string: "nodeex-connect://open?lease=\(id.uppercased())")!), .open(lease: id, mode: .desktop))
        XCTAssertEqual(LinkRequest(URL(string: "nodeex-connect://login")!), .signIn)
        // Anything else is refused: other actions, other modes, things that
        // are not lease identifiers.
        XCTAssertNil(LinkRequest(URL(string: "nodeex-connect://open?lease=../../etc&mode=ssh")!))
        XCTAssertNil(LinkRequest(URL(string: "nodeex-connect://open?lease=\(id)&mode=shell")!))
        XCTAssertNil(LinkRequest(URL(string: "nodeex-connect://run?cmd=ls")!))
        XCTAssertNil(LinkRequest(URL(string: "https://open?lease=\(id)")!))
    }

    func testEvents() {
        let e = CoreEvent.parse(#"{"event":"ready","ssh_port":50022,"vnc_port":50059,"account":"renter","password":"p w","ssh_key":"/k"}"#)
        XCTAssertEqual(e?.event, "ready")
        XCTAssertEqual(e?.sshPort, 50022)
        XCTAssertEqual(e?.password, "p w")
        XCTAssertNil(CoreEvent.parse("not json"))
    }

    func testDesktopURL() {
        let u = Handoff.desktopURL(port: 50059, account: "renter", password: "a b/c@d")
        XCTAssertEqual(u.scheme, "vnc")
        XCTAssertEqual(u.port, 50059)
        XCTAssertEqual(u.user, "renter")
        // Encoded in the address, and the same password once decoded.
        XCTAssertEqual(URLComponents(url: u, resolvingAgainstBaseURL: false)?.password, "a b/c@d")
        XCTAssertFalse(u.absoluteString.contains("a b"))
        XCTAssertNil(Handoff.desktopURL(port: 1, account: nil, password: nil).user)
    }

    func testSSHCommandFile() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let f = try Handoff.sshCommandFile(directory: dir, lease: "L", port: 50022, account: "renter", key: "/path with space/id")
        let s = try String(contentsOf: f, encoding: .utf8)
        XCTAssertTrue(s.contains("-p 50022"))
        XCTAssertTrue(s.contains("'/path with space/id'"))
        XCTAssertTrue(s.contains("renter@127.0.0.1"))
        XCTAssertEqual(Handoff.shellQuote("it's"), #"'it'\''s'"#)
    }

    func testDates() {
        XCTAssertNotNil(CoreCLI.date("2026-11-30T12:00:00Z"))
        XCTAssertNotNil(CoreCLI.date("2026-11-30T12:00:00.123456Z"))
        XCTAssertNotNil(CoreCLI.date("2026-11-30T12:00:00-07:00"))
    }
}
