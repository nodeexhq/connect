import AppKit
import ConnectKit
import Sparkle
import SwiftUI

// The menu-bar app. A renter clicks "Remote desktop" or "Terminal" next to a
// lease on the NVTO website, or picks the lease from this menu; the app
// signs in through the browser if it has to, runs the Go core to reach the
// machine, and opens Screen Sharing or Terminal on it. Before an order, the
// website's "Test the connection" opens nvto-connect://test?reservation=…,
// and the app runs the core's connection test and shows what it found.

/// One lease's connection: the core process and what it has reported.
@MainActor
final class Connection {
    let lease: String
    let process: CoreProcess
    var ready: CoreEvent?
    var path: String?
    /// The result of checking the machine against what was ordered.
    var check: String?
    /// What to open once the machine is ready.
    var pending: [OpenMode] = []

    init(lease: String, process: CoreProcess) {
        self.lease = lease
        self.process = process
    }
}

@MainActor
final class ConnectModel: ObservableObject {
    @Published var signIn: SignInState = .idle
    @Published var test: TestState = .idle
    private(set) var status: SignInStatus?
    private(set) var leases: [Lease] = []
    private(set) var leaseError: String?
    private(set) var connections: [String: Connection] = [:]
    var onChange: (() -> Void)?
    let updates = Updates()

    private var loginProcess: CoreProcess?
    private var testProcess: CoreProcess?
    private var afterSignIn: [() -> Void] = []
    private var timer: Timer?

    enum SignInState: Equatable {
        case idle
        case starting
        case waiting(code: String, url: URL)
        case failed(String)
    }

    enum TestState: Equatable {
        case idle
        /// Signing in first, or reaching the machine.
        case reaching
        case measuring
        case done(TestOutcome)
        /// The test could not finish because the connection is too poor:
        /// a verdict without figures.
        case slow(String)
        case failed(String)
    }

    private var environment: [String: String] {
        var env: [String: String] = [:]
        if let api = ProcessInfo.processInfo.environment["NVTO_API"] { env["NVTO_API"] = api }
        return env
    }

    lazy var cli: CoreCLI? = {
        guard let url = Bundle.main.url(forAuxiliaryExecutable: "nvto-connect")
            ?? Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/nvto-connect") as URL?,
              FileManager.default.isExecutableFile(atPath: url.path) else { return nil }
        return CoreCLI(binary: url, environment: environment)
    }()

    /// Where the app keeps its own files: Terminal scripts and known hosts.
    var supportDirectory: URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        return base.appendingPathComponent("NVTO/connect-app", isDirectory: true)
    }

    var apiOrigin: String { ProcessInfo.processInfo.environment["NVTO_API"] ?? "https://app.nvto.com" }

    func start() {
        updates.start(model: self)
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.refresh() }
        }
    }

    /// Reads the sign-in state and the leases, off the main thread.
    func refresh() {
        guard let cli else { return }
        Task.detached {
            let status = try? cli.status()
            var found: [Lease] = []
            var problem: String?
            if status?.signedIn == true {
                do { found = try cli.leases() } catch { problem = "\(error)" }
            }
            let leases = found, leaseError = problem
            await MainActor.run {
                self.status = status
                self.leases = leases.filter { $0.status == "active" }.sorted { $0.endsAt < $1.endsAt }
                self.leaseError = leaseError
                self.onChange?()
            }
        }
    }

    var isSignedIn: Bool { status?.signedIn == true }

    // MARK: Signing in

    /// Runs `then` once signed in, signing in through the browser first if
    /// needed.
    func whenSignedIn(_ then: @escaping () -> Void) {
        guard let cli else { return }
        Task.detached {
            let status = try? cli.status()
            await MainActor.run {
                self.status = status
                if status?.signedIn == true {
                    then()
                } else {
                    self.afterSignIn.append(then)
                    self.beginSignIn()
                }
            }
        }
    }

    func beginSignIn() {
        guard let cli, loginProcess == nil else { return }
        signIn = .starting
        // The app opens the browser itself, so it knows the page was opened.
        let p = CoreProcess(cli: cli, args: ["login", "-events", "-no-browser"])
        p.onEvent = { [weak self] e in
            guard let self else { return }
            switch e.event {
            case "code":
                if let code = e.userCode, let s = e.verificationURL, let url = URL(string: s), url.scheme == "https" {
                    self.signIn = .waiting(code: code, url: url)
                    NSWorkspace.shared.open(url)
                }
            case "signed_in":
                self.signIn = .idle
                let next = self.afterSignIn
                self.afterSignIn = []
                self.refresh()
                next.forEach { $0() }
            default:
                break
            }
        }
        p.onExit = { [weak self] err in
            guard let self else { return }
            self.loginProcess = nil
            if let err {
                self.signIn = .failed(err)
                self.afterSignIn = []
                // A test waiting for the sign-in will not run now.
                if self.testProcess == nil, self.test == .reaching { self.test = .failed("Could not sign in: \(err)") }
            }
        }
        do {
            try p.start()
            loginProcess = p
        } catch {
            signIn = .failed("\(error)")
        }
    }

    func cancelSignIn() {
        loginProcess?.stop()
        loginProcess = nil
        afterSignIn = []
        signIn = .idle
        if testProcess == nil, test == .reaching { test = .idle }
    }

    func signOut() {
        guard let cli else { return }
        for c in connections.values { c.process.stop() }
        cancelTest()
        Task.detached {
            try? cli.logout()
            await MainActor.run { self.refresh() }
        }
    }

    // MARK: Testing a reserved machine

    /// Tests the connection to a reserved machine, signing in first if
    /// needed. One test at a time: a second link while one runs only
    /// brings its window forward.
    func startTest(reservation: String) {
        if testProcess != nil {
            test = test
            return
        }
        test = .reaching
        whenSignedIn { [weak self] in self?.runTest(reservation: reservation) }
    }

    private func runTest(reservation: String) {
        guard let cli, testProcess == nil, test == .reaching else { return }
        let p = CoreProcess(cli: cli, args: ["test", reservation, "-events"])
        p.onEvent = { [weak self, weak p] e in
            guard let self, let p, self.testProcess === p else { return }
            switch e.event {
            case "test_started":
                self.test = .measuring
            case "test_result":
                if let outcome = TestOutcome(e) { self.test = .done(outcome) }
            case "test_failed":
                let message = e.message ?? "The test could not finish."
                self.test = e.reason == "slow" ? .slow(message) : .failed(message)
            default:
                break
            }
        }
        p.onExit = { [weak self, weak p] err in
            // A test cancelled from its window has already been let go.
            guard let self, let p, self.testProcess === p else { return }
            self.testProcess = nil
            switch self.test {
            case .done, .slow, .failed, .idle:
                // The core has said how it ended.
                break
            default:
                self.test = .failed(err ?? "The test stopped before it finished.")
            }
        }
        do {
            try p.start()
            testProcess = p
        } catch {
            test = .failed("\(error)")
        }
    }

    func cancelTest() {
        if testProcess == nil, test == .reaching, loginProcess != nil { cancelSignIn() }
        testProcess?.stop()
        testProcess = nil
        test = .idle
    }

    // MARK: Connecting

    func open(lease: String, mode: OpenMode) {
        whenSignedIn { [weak self] in self?.connect(lease: lease, mode: mode) }
    }

    private func connect(lease: String, mode: OpenMode) {
        if let c = connections[lease] {
            if c.ready != nil { hand(c, mode) } else if !c.pending.contains(mode) { c.pending.append(mode) }
            return
        }
        guard let cli else { return }
        // Port 0: the core picks free local ports, so several leases can be
        // open at once without clashing.
        let p = CoreProcess(cli: cli, args: ["open", lease, "-events", "-ssh-port", "0", "-vnc-port", "0"])
        let c = Connection(lease: lease, process: p)
        c.pending = [mode]
        p.onEvent = { [weak self, weak c] e in
            guard let self, let c else { return }
            switch e.event {
            case "ready":
                c.ready = e
                // Once, the first time a machine is ready: whose machine it is.
                if e.account != nil, !UserDefaults.standard.bool(forKey: "shownOwnerNotice") {
                    UserDefaults.standard.set(true, forKey: "shownOwnerNotice")
                    self.alert("This Mac belongs to someone else",
                               "It sits in its owner's home or office, and the owner can reach the hardware. "
                               + "Keep files and keys that matter in ~/Private: an encrypted folder only this computer can unlock, "
                               + "locked whenever the machine stops. Not in the machine's keychain: the machine logs you in by itself, "
                               + "so its owner could open that. Use short-lived, limited credentials, and keep your own "
                               + "backups: if the node goes offline, what is on it may be out of reach until it returns.")
                }
                let modes = c.pending
                c.pending = []
                modes.forEach { self.hand(c, $0) }
            case "check":
                if e.verdict == "ok" {
                    c.check = "Matches what you ordered"
                } else {
                    let problems = e.problems ?? []
                    c.check = "Does not match what you ordered"
                    self.alert("This machine does not match what you ordered",
                               problems.map { "• " + $0 }.joined(separator: "\n")
                               + (e.refunded == true
                                  ? "\n\nNVTO has ended the lease and refunded its rent in full" + (e.refund.map { " (\($0))" } ?? "") + " to your balance."
                                  : "\n\nNVTO has been told. An automatic refund applies only to a mismatch found in a new lease's first hour, at most twice in 30 days; this lease goes on."))
                }
            case "path":
                if e.direct == true {
                    c.path = "direct" + (e.latencyMs.map { String(format: ", %.0f ms", $0) } ?? "")
                } else {
                    c.path = "relayed" + (e.relay.map { " via \($0)" } ?? "") + (e.latencyMs.map { String(format: ", %.0f ms", $0) } ?? "")
                }
            default:
                break
            }
            self.onChange?()
        }
        p.onExit = { [weak self] err in
            guard let self else { return }
            self.connections[lease] = nil
            self.onChange?()
            if let err { self.alert("Could not connect", err) }
        }
        do {
            try p.start()
            connections[lease] = c
        } catch {
            alert("Could not connect", "\(error)")
        }
        onChange?()
    }

    private func hand(_ c: Connection, _ mode: OpenMode) {
        guard let r = c.ready else { return }
        switch mode {
        case .desktop:
            guard let port = r.vncPort else { return }
            NSWorkspace.shared.open(Handoff.desktopURL(port: port, account: r.account, password: r.password, desktopPassword: r.desktopPassword))
        case .ssh:
            guard let port = r.sshPort else { return }
            do {
                let file = try Handoff.sshCommandFile(directory: supportDirectory, lease: c.lease, port: port, account: r.account, key: r.sshKey)
                let terminal = URL(fileURLWithPath: "/System/Applications/Utilities/Terminal.app")
                NSWorkspace.shared.open([file], withApplicationAt: terminal, configuration: NSWorkspace.OpenConfiguration())
            } catch {
                alert("Could not open Terminal", "\(error)")
            }
        }
    }

    func disconnect(_ lease: String) {
        connections[lease]?.process.stop()
    }

    func copyPassword(_ lease: String) {
        copy(connections[lease]?.ready?.password)
    }

    func copyDesktopPassword(_ lease: String) {
        copy(connections[lease]?.ready?.desktopPassword)
    }

    private func copy(_ secret: String?) {
        guard let secret else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(secret, forType: .string)
    }

    func disconnectAll() {
        for c in connections.values { c.process.stop() }
        testProcess?.stop()
    }

    var hasConnections: Bool { !connections.isEmpty }

    func alert(_ title: String, _ text: String) {
        NSApp.activate(ignoringOtherApps: true)
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        a.runModal()
    }
}

/// Sparkle, as in the NVTO Node app: an update waits while a connection is open
/// and is installed once none is.
@MainActor
final class Updates: NSObject, SPUUpdaterDelegate {
    private var controller: SPUStandardUpdaterController?
    private weak var model: ConnectModel?
    private var resume: (() -> Void)?

    var available: Bool { controller != nil }

    func start(model: ConnectModel) {
        self.model = model
        guard controller == nil, Bundle.main.object(forInfoDictionaryKey: "SUFeedURL") != nil else { return }
        controller = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: self, userDriverDelegate: nil)
    }

    func check() { controller?.checkForUpdates(nil) }

    func connectionsClosed() {
        resume?()
        resume = nil
    }

    nonisolated func updater(_ updater: SPUUpdater, willInstallUpdateOnQuit item: SUAppcastItem, immediateInstallationBlock immediateInstallHandler: @escaping () -> Void) -> Bool {
        MainActor.assumeIsolated {
            if model?.hasConnections == true {
                resume = immediateInstallHandler
            } else {
                DispatchQueue.main.async { immediateInstallHandler() }
            }
            return true
        }
    }
}

/// The small window shown while the browser sign-in is waiting.
struct SignInView: View {
    @ObservedObject var model: ConnectModel

    var body: some View {
        VStack(spacing: 14) {
            Text("Sign in to NVTO").font(.title2.bold())
            switch model.signIn {
            case .idle, .starting:
                ProgressView()
                Text("Opening your browser…").foregroundStyle(.secondary)
            case .waiting(let code, let url):
                Text("Your browser has opened app.nvto.com. Approve the sign-in there if the page shows this code:")
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                Text(code)
                    .font(.system(size: 30, weight: .semibold, design: .monospaced))
                    .tracking(3)
                    .textSelection(.enabled)
                Text("NVTO Connect never asks for your password.").font(.footnote).foregroundStyle(.secondary)
                HStack {
                    Button("Open the page again") { NSWorkspace.shared.open(url) }
                    Button("Cancel") { model.cancelSignIn() }
                }
            case .failed(let message):
                Text(message).multilineTextAlignment(.center).foregroundStyle(.red)
                HStack {
                    Button("Try again") { model.beginSignIn() }
                    Button("Close") { model.cancelSignIn() }
                }
            }
        }
        .padding(24)
        .frame(width: 400)
    }
}

/// The small window of a connection test: progress, then the verdict.
struct TestView: View {
    @ObservedObject var model: ConnectModel

    var body: some View {
        VStack(spacing: 14) {
            Text("Connection test").font(.title2.bold())
            switch model.test {
            case .idle, .reaching:
                ProgressView()
                Text(model.signIn == .idle ? "Reaching the reserved machine…" : "Sign in to run the test.")
                    .foregroundStyle(.secondary)
                Button("Cancel") { model.cancelTest() }
            case .measuring:
                ProgressView()
                Text("Measuring the connection. This takes about ten seconds.")
                    .multilineTextAlignment(.center)
                    .foregroundStyle(.secondary)
                Button("Cancel") { model.cancelTest() }
            case .done(let r):
                Text(r.headline)
                    .font(.system(size: 26, weight: .semibold))
                    .foregroundStyle(r.verdict == "smooth" ? Color.green : r.verdict == "may_lag" ? Color.orange : Color.primary)
                Text(r.explanation)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 4) {
                    GridRow { Text("Latency").foregroundStyle(.secondary); Text(r.latency) }
                    GridRow { Text("Jitter").foregroundStyle(.secondary); Text(r.jitter) }
                    GridRow { Text("Download").foregroundStyle(.secondary); Text(r.down) }
                    GridRow { Text("Upload").foregroundStyle(.secondary); Text(r.up) }
                    GridRow { Text("Path").foregroundStyle(.secondary); Text(r.path) }
                }
                .font(.body.monospacedDigit())
                Text("Go back to the browser to confirm the order or try another machine.")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                Button("Close") { model.cancelTest() }
                    .keyboardShortcut(.defaultAction)
            case .slow(let message):
                Text("May lag")
                    .font(.system(size: 26, weight: .semibold))
                    .foregroundStyle(Color.orange)
                Text(message)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                Text("Go back to the browser to try another machine.")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                Button("Close") { model.cancelTest() }
                    .keyboardShortcut(.defaultAction)
            case .failed(let message):
                Text("The test could not be run").font(.headline)
                Text(message)
                    .multilineTextAlignment(.center)
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
                Text("Go back to the browser to try again or try another machine.")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                Button("Close") { model.cancelTest() }
            }
        }
        .padding(24)
        .frame(width: 400)
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    let model = MainActor.assumeIsolated { ConnectModel() }
    private var statusItem: NSStatusItem!
    private var signInWindow: NSWindow?
    private var testWindow: NSWindow?
    private var observation: Any?
    private var testObservation: Any?

    func applicationDidFinishLaunching(_ notification: Notification) {
        MainActor.assumeIsolated {
            statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
            updateIcon()
            statusItem.button?.setAccessibilityLabel("NVTO Connect")
            let menu = NSMenu()
            menu.delegate = self
            statusItem.menu = menu
            model.onChange = { [weak self] in
                self?.updateIcon()
                if self?.model.hasConnections == false { self?.model.updates.connectionsClosed() }
            }
            observation = model.$signIn.sink { [weak self] state in
                DispatchQueue.main.async { self?.showSignIn(state) }
            }
            testObservation = model.$test.sink { [weak self] state in
                DispatchQueue.main.async { self?.showTest(state) }
            }
            if model.cli == nil {
                model.alert("NVTO Connect is damaged", "The nvto-connect program was not found inside the app. Download the app again from nvto.com/download.")
            }
            model.start()
        }
    }

    /// Links from the NVTO website: nvto-connect://open?lease=…&mode=….
    func application(_ application: NSApplication, open urls: [URL]) {
        MainActor.assumeIsolated {
            for url in urls {
                switch LinkRequest(url) {
                case .open(let lease, let mode)?: model.open(lease: lease, mode: mode)
                case .test(let reservation)?: model.startTest(reservation: reservation)
                case .signIn?: model.whenSignedIn {}
                case nil: break
                }
            }
        }
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        MainActor.assumeIsolated { model.disconnectAll() }
        return .terminateNow
    }

    @MainActor private func updateIcon() {
        // The NVTO mark, with a dot while a connection is open.
        statusItem.button?.image = MenuBarMark.image(badge: model.hasConnections ? .dot : .none)
    }

    @MainActor private func showSignIn(_ state: ConnectModel.SignInState) {
        if state == .idle {
            signInWindow?.close()
            signInWindow = nil
            return
        }
        if signInWindow == nil {
            let w = NSWindow(contentViewController: NSHostingController(rootView: SignInView(model: model)))
            w.title = "NVTO Connect"
            w.styleMask = [.titled, .closable]
            w.isReleasedWhenClosed = false
            w.center()
            signInWindow = w
            NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: w, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated {
                    if self?.model.signIn != .idle { self?.model.cancelSignIn() }
                    self?.signInWindow = nil
                }
            }
        }
        NSApp.activate(ignoringOtherApps: true)
        signInWindow?.makeKeyAndOrderFront(nil)
    }

    @MainActor private func showTest(_ state: ConnectModel.TestState) {
        if state == .idle {
            testWindow?.close()
            testWindow = nil
            return
        }
        if testWindow == nil {
            let w = NSWindow(contentViewController: NSHostingController(rootView: TestView(model: model)))
            w.title = "NVTO Connect"
            w.styleMask = [.titled, .closable]
            w.isReleasedWhenClosed = false
            w.center()
            testWindow = w
            NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: w, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated {
                    self?.testWindow = nil
                    if self?.model.test != .idle { self?.model.cancelTest() }
                }
            }
        }
        // The sign-in window comes first while it is up.
        if signInWindow == nil {
            NSApp.activate(ignoringOtherApps: true)
            testWindow?.makeKeyAndOrderFront(nil)
        } else {
            testWindow?.orderFront(nil)
        }
    }

    // MARK: The menu

    func menuWillOpen(_ menu: NSMenu) {
        MainActor.assumeIsolated {
            build(menu)
            model.refresh()
        }
    }

    @MainActor private func build(_ menu: NSMenu) {
        menu.removeAllItems()
        // The version, so a renter can see an update arrive.
        let version = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "development build"
        menu.addItem(disabled("NVTO Connect \(version)"))
        menu.addItem(.separator())
        if let status = model.status, status.signedIn {
            menu.addItem(disabled("Signed in as \(status.email ?? "your account")"))
        } else {
            menu.addItem(item("Sign in…") { [weak self] in self?.model.whenSignedIn {} })
        }
        menu.addItem(.separator())

        if model.isSignedIn {
            if model.leases.isEmpty {
                menu.addItem(disabled(model.leaseError.map { "Could not load leases: \($0)" } ?? "No active leases"))
                menu.addItem(item("Rent a Mac…") { [weak self] in self?.openWeb("/market") })
            } else {
                menu.addItem(disabled("Your leases"))
                for lease in model.leases { menu.addItem(leaseItem(lease)) }
            }
            menu.addItem(.separator())
        }
        menu.addItem(item("Open the NVTO console") { [weak self] in self?.openWeb("/console") })
        if model.updates.available {
            menu.addItem(item("Check for updates…") { [weak self] in self?.model.updates.check() })
        }
        if model.isSignedIn {
            menu.addItem(item("Sign out") { [weak self] in self?.model.signOut() })
        }
        menu.addItem(.separator())
        menu.addItem(item("Quit NVTO Connect") { NSApp.terminate(nil) })
    }

    @MainActor private func leaseItem(_ lease: Lease) -> NSMenuItem {
        let ends = lease.endsAt.formatted(date: .abbreviated, time: .shortened)
        let conn = model.connections[lease.id]
        var title = "Mac \(lease.id.prefix(8)) · ends \(ends)"
        if let conn { title += conn.ready == nil ? " · connecting…" : " · connected" }
        let top = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        let sub = NSMenu()
        sub.addItem(item("Open remote desktop") { [weak self] in self?.model.open(lease: lease.id, mode: .desktop) })
        sub.addItem(item("Open Terminal (SSH)") { [weak self] in self?.model.open(lease: lease.id, mode: .ssh) })
        if let conn {
            sub.addItem(.separator())
            if let path = conn.path { sub.addItem(disabled("Connection: \(path)")) }
            if let check = conn.check { sub.addItem(disabled("Machine check: \(check)")) }
            if let r = conn.ready {
                if let host = r.sshHost {
                    // Remote development over SSH: the editors open the
                    // machine by its name in ~/.ssh/config.
                    for (name, bundle, scheme) in [("VS Code", "com.microsoft.VSCode", "vscode"), ("Cursor", "com.todesktop.230313mzl4w4u92", "cursor")]
                    where NSWorkspace.shared.urlForApplication(withBundleIdentifier: bundle) != nil {
                        sub.addItem(item("Open in \(name)") {
                            if let url = URL(string: "\(scheme)://vscode-remote/ssh-remote+\(host)/Users/\(r.account ?? "renter")") { NSWorkspace.shared.open(url) }
                        })
                    }
                    sub.addItem(item("Copy SSH command (ssh \(host))") {
                        NSPasteboard.general.clearContents()
                        NSPasteboard.general.setString("ssh \(host)", forType: .string)
                    })
                }
                if let account = r.account { sub.addItem(disabled("Account: \(account)")) }
                // The encrypted folder only this computer can unlock.
                if let folder = r.privateFolder { sub.addItem(disabled("Private folder: \(folder) (encrypted)")) }
                else if r.privateNote != nil { sub.addItem(disabled("Private folder: not available")) }
                if r.desktopPassword != nil {
                    // The node serves the desktop and logs the account in:
                    // "Open remote desktop" needs no password. The desktop's
                    // is for other viewers; the account's is what System
                    // Settings, installers and the login window ask for.
                    sub.addItem(item("Copy remote desktop password") { [weak self] in self?.model.copyDesktopPassword(lease.id) })
                    if r.password != nil {
                        sub.addItem(item("Copy account password (for System Settings)") { [weak self] in self?.model.copyPassword(lease.id) })
                    }
                } else if r.password != nil {
                    // An older node: the account's password is also the
                    // desktop's.
                    sub.addItem(item("Copy desktop password") { [weak self] in self?.model.copyPassword(lease.id) })
                }
            }
            sub.addItem(item("Disconnect") { [weak self] in self?.model.disconnect(lease.id) })
        }
        top.submenu = sub
        return top
    }

    @MainActor private func openWeb(_ path: String) {
        if let url = URL(string: model.apiOrigin + path) { NSWorkspace.shared.open(url) }
    }

    private func disabled(_ title: String) -> NSMenuItem {
        let i = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        i.isEnabled = false
        return i
    }

    private func item(_ title: String, _ action: @escaping () -> Void) -> NSMenuItem {
        let i = NSMenuItem(title: title, action: #selector(ClosureTarget.fire), keyEquivalent: "")
        let target = ClosureTarget(action)
        i.target = target
        i.representedObject = target // keeps the target alive with the item
        return i
    }
}

final class ClosureTarget: NSObject {
    let action: () -> Void
    init(_ action: @escaping () -> Void) { self.action = action }
    @objc func fire() { MainActor.assumeIsolated { action() } }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
