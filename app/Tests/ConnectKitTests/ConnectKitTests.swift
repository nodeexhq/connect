import XCTest
@testable import ConnectKit

final class ConnectKitTests: XCTestCase {
    func testLinks() {
        let id = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
        XCTAssertEqual(LinkRequest(URL(string: "nvto-connect://open?lease=\(id)&mode=ssh")!), .open(lease: id, mode: .ssh))
        XCTAssertEqual(LinkRequest(URL(string: "nvto-connect://open?lease=\(id.uppercased())")!), .open(lease: id, mode: .desktop))
        XCTAssertEqual(LinkRequest(URL(string: "nvto-connect://login")!), .signIn)
        XCTAssertEqual(LinkRequest(URL(string: "nvto-connect://test?reservation=\(id.uppercased())")!), .test(reservation: id))
        XCTAssertNil(LinkRequest(URL(string: "nvto-connect://test?reservation=../leases")!))
        XCTAssertNil(LinkRequest(URL(string: "nvto-connect://test?lease=\(id)")!))
        // Anything else is refused: other actions, other modes, things that
        // are not lease identifiers.
        XCTAssertNil(LinkRequest(URL(string: "nvto-connect://open?lease=../../etc&mode=ssh")!))
        XCTAssertNil(LinkRequest(URL(string: "nvto-connect://open?lease=\(id)&mode=shell")!))
        XCTAssertNil(LinkRequest(URL(string: "nvto-connect://run?cmd=ls")!))
        XCTAssertNil(LinkRequest(URL(string: "https://open?lease=\(id)")!))
    }

    func testEvents() {
        let e = CoreEvent.parse(#"{"event":"ready","ssh_port":50022,"vnc_port":50059,"account":"renter","password":"p w","ssh_key":"/k"}"#)
        XCTAssertEqual(e?.event, "ready")
        XCTAssertEqual(e?.sshPort, 50022)
        XCTAssertEqual(e?.password, "p w")
        XCTAssertNil(CoreEvent.parse("not json"))
        let c = CoreEvent.parse(#"{"event":"check","verdict":"mismatch","problems":["8 GB of memory, the node claims 12 GB"]}"#)
        XCTAssertEqual(c?.verdict, "mismatch")
        XCTAssertEqual(c?.problems?.count, 1)
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

    func testTestOutcome() {
        let e = CoreEvent.parse(#"{"event":"test_result","latency_ms":38.2,"jitter_ms":4.1,"down_mbps":52.3,"up_mbps":8.44,"direct":true,"relay":"","verdict":"smooth"}"#)!
        let r = TestOutcome(e)!
        XCTAssertEqual(r.headline, "Smooth")
        XCTAssertEqual(r.latency, "38 ms")
        XCTAssertEqual(r.jitter, "4 ms")
        XCTAssertEqual(r.down, "52 Mbit/s")
        XCTAssertEqual(r.up, "8.4 Mbit/s")
        XCTAssertEqual(r.path, "Direct")
        XCTAssertNil(r.relay)
        XCTAssertTrue(r.explanation.contains("responsive"))

        let lag = TestOutcome(CoreEvent.parse(#"{"event":"test_result","latency_ms":180,"jitter_ms":41,"down_mbps":3,"up_mbps":1,"direct":false,"relay":"fra","verdict":"may_lag"}"#)!)!
        XCTAssertEqual(lag.headline, "May lag")
        XCTAssertEqual(lag.path, "Relayed via fra")
        XCTAssertTrue(lag.explanation.contains("relay"))
        XCTAssertEqual(TestOutcome(CoreEvent.parse(#"{"event":"test_result","latency_ms":80,"jitter_ms":20,"down_mbps":12,"up_mbps":5,"direct":true,"verdict":"ok"}"#)!)?.headline, "Usable")

        let failed = CoreEvent.parse(#"{"event":"test_failed","reason":"slow","message":"too slow"}"#)
        XCTAssertEqual(failed?.reason, "slow")
        XCTAssertEqual(failed?.message, "too slow")
        XCTAssertNil(TestOutcome(failed!))

        // Not a result, or one with a figure missing.
        XCTAssertNil(TestOutcome(CoreEvent.parse(#"{"event":"test_started"}"#)!))
        XCTAssertNil(TestOutcome(CoreEvent.parse(#"{"event":"test_result","verdict":"ok"}"#)!))
    }
}
