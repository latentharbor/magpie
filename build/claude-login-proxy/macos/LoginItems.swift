import AppKit
import ServiceManagement
import SwiftUI

@MainActor final class DockVisibility: ObservableObject {
    static let shared = DockVisibility()
    static let preferenceKey = "showInDock"
    @Published private(set) var shown: Bool
    @Published private(set) var error = ""
    private let defaults: UserDefaults
    private let applyPolicy: (Bool) -> Bool

    init(defaults: UserDefaults = .standard, applyPolicy: @escaping (Bool) -> Bool = { NSApplication.shared.setActivationPolicy($0 ? .regular : .accessory) }) {
        self.defaults = defaults
        self.applyPolicy = applyPolicy
        shown = defaults.object(forKey: Self.preferenceKey) == nil ? true : defaults.bool(forKey: Self.preferenceKey)
    }

    func applyStoredPreference() { setShown(shown) }

    func setShown(_ value: Bool) {
        guard applyPolicy(value) else {
            error = "macOS 未能更改 Dock 显示状态，请重新打开设置应用后再试。"
            return
        }
        error = ""
        shown = value
        defaults.set(value, forKey: Self.preferenceKey)
    }
}

enum LoginItemState: String {
    case disabled, enabled, needsApproval, unavailable

    var requested: Bool { self == .enabled || self == .needsApproval }
    var label: String {
        switch self {
        case .disabled: return "未开启"
        case .enabled: return "已开启"
        case .needsApproval: return "等待系统批准"
        case .unavailable: return "不可用，请从应用程序目录启动"
        }
    }

    init(_ state: SMAppService.Status) {
        switch state {
        case .notRegistered: self = .disabled
        case .enabled: self = .enabled
        case .requiresApproval: self = .needsApproval
        case .notFound: self = .unavailable
        @unknown default: self = .unavailable
        }
    }
}

@MainActor protocol LoginItemService {
    var state: LoginItemState { get }
    func enable() throws
    func disable() throws
}

@MainActor struct SystemLoginItemService: LoginItemService {
    var state: LoginItemState { LoginItemState(SMAppService.mainApp.status) }
    func enable() throws { try SMAppService.mainApp.register() }
    func disable() throws { try SMAppService.mainApp.unregister() }
}

@MainActor final class LoginItems: ObservableObject {
    @Published private(set) var state: LoginItemState = .disabled
    @Published private(set) var error = ""
    private let service: any LoginItemService

    init(service: any LoginItemService) { self.service = service; refresh() }
    convenience init() { self.init(service: SystemLoginItemService()) }

    func refresh() { state = service.state }

    func setEnabled(_ enabled: Bool) {
        error = ""
        do {
            if enabled && !service.state.requested { try service.enable() }
            if !enabled && service.state.requested { try service.disable() }
        } catch { self.error = error.localizedDescription }
        // Read back macOS consent; never equate a request with approval.
        refresh()
    }

    static func openSystemSettings() { SMAppService.openSystemSettingsLoginItems() }
}

struct LoginSettings: View {
    @StateObject private var items = LoginItems()
    @ObservedObject private var dock = DockVisibility.shared
    @State private var proxyStatus = "正在读取"

    var body: some View {
        Section {
            Toggle("登录时启动应用", isOn: Binding(get: { items.state.requested }, set: { items.setEnabled($0) }))
            LabeledContent("应用自启", value: items.state.label)
            if !items.error.isEmpty { Text(items.error).foregroundStyle(.orange).textSelection(.enabled) }
            if items.state == .needsApproval {
                Text("请在系统设置的登录项中允许 Claude Login Proxy，然后返回这里。")
                    .foregroundStyle(.secondary)
            }
            LabeledContent("后台代理自启", value: proxyStatus)
            Button("打开系统登录项设置") { LoginItems.openSystemSettings() }
            Toggle("在 Dock 中显示", isOn: Binding(get: { dock.shown }, set: { dock.setShown($0) }))
            if !dock.error.isEmpty { Text(dock.error).foregroundStyle(.orange) }
        } header: { Text("启动与窗口") } footer: {
            Text("关闭 Dock 显示后，可从菜单栏重新打开窗口；此设置立即生效并在下次启动时保留。登录自启开关只控制设置应用，退出设置应用不会停止后台代理。")
        }
        .onAppear { refresh() }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in refresh() }
    }

    private func refresh() {
        items.refresh()
        if !FileManager.default.fileExists(atPath: Helper.plist.path) {
            proxyStatus = "未安装"
        } else {
            proxyStatus = LoginItemState(SMAppService.statusForLegacyPlist(at: Helper.plist)).label
        }
    }
}

// Only the settings application owns this delegate. It never stops the
// independently launched proxy, even on explicit application termination.
@MainActor final class ProxyAppDelegate: NSObject, NSApplicationDelegate {
    static let settingsID = NSUserInterfaceItemIdentifier("claude-proxy-settings")

    func applicationDidFinishLaunching(_ notification: Notification) {
        DockVisibility.shared.applyStoredPreference()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
        if let window = sender.windows.first(where: { $0.identifier == Self.settingsID }) {
            window.makeKeyAndOrderFront(nil)
            return false
        }
        return true // WindowGroup recreates a window when its last one was closed.
    }
}

struct SettingsWindowIdentity: NSViewRepresentable {
    final class Marker: NSView {
        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            window?.identifier = ProxyAppDelegate.settingsID
        }
    }
    func makeNSView(context: Context) -> Marker { Marker() }
    func updateNSView(_ nsView: Marker, context: Context) {}
}
