import AppKit
import ConnectKit
import Sparkle
import SwiftUI

// The menu-bar app. A renter clicks "Remote desktop" or "Terminal" next to a
// lease on the NodeEx website, or picks the lease from this menu; the app
// signs in through the browser if it has to, runs the Go core to reach the
// machine, and opens Screen Sharing or Terminal on it.

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
    private(set) var status: SignInStatus?
    private(set) var leases: [Lease] = []
    private(set) var leaseError: String?
    private(set) var connections: [String: Connection] = [:]
    var onChange: (() -> Void)?
    let updates = Updates()

    private var loginProcess: CoreProcess?
    private var afterSignIn: [() -> Void] = []
    private var timer: Timer?

    enum SignInState: Equatable {
        case idle
        case starting
        case waiting(code: String, url: URL)
        case failed(String)
    }

    private var environment: [String: String] {
        var env: [String: String] = [:]
        if let api = ProcessInfo.processInfo.environment["NODEEX_API"] { env["NODEEX_API"] = api }
        return env
    }

    lazy var cli: CoreCLI? = {
        guard let url = Bundle.main.url(forAuxiliaryExecutable: "nodeex-connect")
            ?? Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/nodeex-connect") as URL?,
              FileManager.default.isExecutableFile(atPath: url.path) else { return nil }
        return CoreCLI(binary: url, environment: environment)
    }()

    /// Where the app keeps its own files: Terminal scripts and known hosts.
    var supportDirectory: URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        return base.appendingPathComponent("NodeEx/connect-app", isDirectory: true)
    }

    var apiOrigin: String { ProcessInfo.processInfo.environment["NODEEX_API"] ?? "https://app.nodeex.com" }

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
    }

    func signOut() {
        guard let cli else { return }
        for c in connections.values { c.process.stop() }
        Task.detached {
            try? cli.logout()
            await MainActor.run { self.refresh() }
        }
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
                               + "\n\nNodeEx has been told. If it is not what you ordered, write to hello@nodeex.com about a refund.")
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
            NSWorkspace.shared.open(Handoff.desktopURL(port: port, account: r.account, password: r.password))
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
        guard let pw = connections[lease]?.ready?.password else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(pw, forType: .string)
    }

    func disconnectAll() {
        for c in connections.values { c.process.stop() }
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

/// Sparkle, as in the Miner app: an update waits while a connection is open
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
            Text("Sign in to NodeEx").font(.title2.bold())
            switch model.signIn {
            case .idle, .starting:
                ProgressView()
                Text("Opening your browser…").foregroundStyle(.secondary)
            case .waiting(let code, let url):
                Text("Your browser has opened app.nodeex.com. Approve the sign-in there if the page shows this code:")
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                Text(code)
                    .font(.system(size: 30, weight: .semibold, design: .monospaced))
                    .tracking(3)
                    .textSelection(.enabled)
                Text("NodeEx Connect never asks for your password.").font(.footnote).foregroundStyle(.secondary)
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

final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    let model = MainActor.assumeIsolated { ConnectModel() }
    private var statusItem: NSStatusItem!
    private var signInWindow: NSWindow?
    private var observation: Any?

    func applicationDidFinishLaunching(_ notification: Notification) {
        MainActor.assumeIsolated {
            statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
            updateIcon()
            statusItem.button?.setAccessibilityLabel("NodeEx Connect")
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
            if model.cli == nil {
                model.alert("NodeEx Connect is damaged", "The nodeex-connect program was not found inside the app. Download the app again from nodeex.com/download.")
            }
            model.start()
        }
    }

    /// Links from the NodeEx website: nodeex-connect://open?lease=…&mode=….
    func application(_ application: NSApplication, open urls: [URL]) {
        MainActor.assumeIsolated {
            for url in urls {
                switch LinkRequest(url) {
                case .open(let lease, let mode)?: model.open(lease: lease, mode: mode)
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
        // The NodeEx mark, with a dot while a connection is open.
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
            w.title = "NodeEx Connect"
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
        menu.addItem(disabled("NodeEx Connect \(version)"))
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
        menu.addItem(item("Open the NodeEx console") { [weak self] in self?.openWeb("/console") })
        if model.updates.available {
            menu.addItem(item("Check for updates…") { [weak self] in self?.model.updates.check() })
        }
        if model.isSignedIn {
            menu.addItem(item("Sign out") { [weak self] in self?.model.signOut() })
        }
        menu.addItem(.separator())
        menu.addItem(item("Quit NodeEx Connect") { NSApp.terminate(nil) })
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
                if let account = r.account { sub.addItem(disabled("Account: \(account)")) }
                if r.password != nil {
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
