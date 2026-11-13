import AppKit
import SwiftUI

@MainActor final class FakeLoginItemService: LoginItemService {
    var state: LoginItemState = .disabled
    var fail = false
    var enableCount = 0
    func enable() throws {
        enableCount += 1
        if fail { throw ClientError(message: "fixture denied registration") }
        state = .needsApproval
    }
    func disable() throws { state = .disabled }
}

// CI only: exercise editor documents and render every native page at the
// minimum supported window size. No daemon, real config, or network is used.
@main struct UIValidation {
    @MainActor static func main() throws {
        _ = NSApplication.shared
        let suite = "proxy-dock-fixture-" + UUID().uuidString
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        var refusePolicy = false
        var applied: [Bool] = []
        let dock = DockVisibility(defaults: defaults, applyPolicy: { value in
            applied.append(value)
            return !refusePolicy
        })
        dock.setShown(false)
        precondition(!dock.shown && defaults.object(forKey: DockVisibility.preferenceKey) != nil)
        let reopenedDock = DockVisibility(defaults: defaults, applyPolicy: { value in applied.append(value); return true })
        reopenedDock.applyStoredPreference()
        precondition(!reopenedDock.shown && applied == [false, false], "hidden Dock preference was not restored on launch")
        refusePolicy = true
        dock.setShown(true)
        precondition(!dock.shown && !defaults.bool(forKey: DockVisibility.preferenceKey) && !dock.error.isEmpty, "failed policy change was saved as successful")
        refusePolicy = false
        dock.setShown(true)
        precondition(dock.shown && defaults.bool(forKey: DockVisibility.preferenceKey) && dock.error.isEmpty)
        let service = FakeLoginItemService()
        let loginItems = LoginItems(service: service)
        service.fail = true
        loginItems.setEnabled(true)
        precondition(loginItems.state == .disabled && !loginItems.error.isEmpty, "failed registration was presented as enabled")
        service.fail = false
        loginItems.setEnabled(true)
        precondition(loginItems.state == .needsApproval, "approval was assumed")
        service.state = .enabled
        loginItems.refresh()
        precondition(loginItems.state == .enabled)
        loginItems.setEnabled(true)
        precondition(service.enableCount == 2, "already registered service was registered again")
        service.state = .needsApproval
        loginItems.refresh()
        precondition(loginItems.state == .needsApproval, "external revocation was missed")
        loginItems.setEnabled(false)
        precondition(loginItems.state == .disabled)
        let lifecycle = ProxyAppDelegate()
        precondition(!lifecycle.applicationShouldTerminateAfterLastWindowClosed(NSApplication.shared))
        let reopen = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 300, height: 200), styleMask: [.titled, .closable], backing: .buffered, defer: false)
        reopen.identifier = ProxyAppDelegate.settingsID
        reopen.orderOut(nil)
        precondition(!lifecycle.applicationShouldHandleReopen(NSApplication.shared, hasVisibleWindows: false))
        precondition(reopen.isVisible, "Dock reopen did not restore the existing window")
        reopen.orderOut(nil)
        let model = ProxyModel()
        model.preview = true
        var p = Preferences.empty
        p.exists = true; p.enabled = true; p.gatewayUsage = true
        p.servers = [ServerEntry(id: "nas", name: "Home NAS", url: "http://192.0.2.10:3425", hasKey: true)]
        p.models = ["claude-opus-5-5": "nas/team/claude-opus-5-5", "claude-sonnet-5-5": "nas/team/claude-sonnet-5-5"]
        p.imageUpstream = "nas"
        model.apply(p)
        precondition(!model.dirty, "loaded settings must not be dirty")
        model.preferences.servers[0].name = "Edited NAS"
        precondition(model.dirty)
        let draft = try model.document()
        precondition(draft.models == p.models && draft.servers[0].key == nil, "name edit altered mapping or exposed key")
        model.mappings.append(model.mappings[0])
        do { _ = try model.document(); fatalError("duplicate native model accepted") } catch {}
        model.apply(p)
        model.removeServer("nas")
        precondition(model.preferences.servers.count == 1, "referenced server removed")
        model.message = ""
        model.status = ServiceStatus(running: true, activeRequests: 0, version: "0.2.2", listen: p.listen)
        model.statusMessage = "后台代理正在运行"
        model.certificates = ["api": CertificateInfo(path: "/example/ca.pem", exists: true, trusted: true, sha256: String(repeating: "a1", count: 32))]
        let directory = URL(fileURLWithPath: CommandLine.arguments[1])
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        for dark in [false, true] {
            for (index, page) in Page.allCases.enumerated() {
                model.page = page
                let host = NSHostingView(rootView: ProxyWindow(model: model))
                host.frame = NSRect(x: 0, y: 0, width: 1000, height: 720)
                host.appearance = NSAppearance(named: dark ? .darkAqua : .aqua)
                let window = NSWindow(contentRect: host.frame, styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
                window.contentView = host
                window.orderFront(nil)
                RunLoop.main.run(until: Date(timeIntervalSinceNow: 0.5))
                host.layoutSubtreeIfNeeded()
                guard let bitmap = host.bitmapImageRepForCachingDisplay(in: host.bounds) else { fatalError("native view did not render") }
                host.cacheDisplay(in: host.bounds, to: bitmap)
                guard let png = bitmap.representation(using: .png, properties: [:]) else { fatalError("no native screenshot") }
                try png.write(to: directory.appendingPathComponent("\(dark ? "dark" : "light")-\(index).png"))
                window.orderOut(nil)
            }
        }
        print("Native editor validation passed; eight page snapshots written.")
    }
}
