import AppKit
import SwiftUI

@main @MainActor struct ClaudeProxyApp: App {
    @NSApplicationDelegateAdaptor(ProxyAppDelegate.self) private var delegate
    @StateObject private var model = ProxyModel()

    init() {
        // Read or change this app's login item without touching the proxy.
        // Useful to deployments; the ordinary UI uses the same controller.
        if let index = CommandLine.arguments.firstIndex(of: "--login-item"), CommandLine.arguments.count > index + 1 {
            let items = LoginItems()
            switch CommandLine.arguments[index + 1] {
            case "enable": items.setEnabled(true)
            case "disable": items.setEnabled(false)
            case "status": break
            default: fputs("login-item: enable, disable or status\n", stderr); exit(2)
            }
            let result = ["state": items.state.rawValue, "error": items.error]
            let data = try! JSONSerialization.data(withJSONObject: result, options: [.sortedKeys])
            print(String(decoding: data, as: UTF8.self))
            exit(items.error.isEmpty ? 0 : 1)
        }
    }

    var body: some Scene {
        WindowGroup("Claude Login Proxy", id: "settings") {
            ProxyWindow(model: model)
                .background(SettingsWindowIdentity())
                .task { if !model.loaded { await model.load() } }
        }
        .defaultSize(width: 1000, height: 720)
        .commands {
            CommandGroup(replacing: .newItem) {}
            CommandGroup(replacing: .appSettings) {
                OpenSettingsWindow()
            }
            CommandMenu("代理") {
                Button("刷新状态") { Task { await model.refreshStatus() } }
                Button("保存配置") { Task { await model.save() } }.disabled(!model.dirty || model.busy)
                Button("查看运行日志") { model.readLogs() }
            }
        }
        MenuBarExtra("Claude Login Proxy", systemImage: "arrow.triangle.branch") {
            OpenSettingsWindow()
            Text(model.statusMessage)
            Button("刷新状态") { Task { await model.refreshStatus() } }
            Divider()
            Button("退出设置应用") { NSApplication.shared.terminate(nil) }
        }
    }
}

struct OpenSettingsWindow: View {
    @Environment(\.openWindow) private var openWindow
    var body: some View {
        Button("打开配置窗口…") {
            if let window = NSApplication.shared.windows.first(where: { $0.identifier == ProxyAppDelegate.settingsID }) {
                window.makeKeyAndOrderFront(nil)
            } else {
                openWindow(id: "settings")
            }
            NSApplication.shared.activate(ignoringOtherApps: true)
        }.keyboardShortcut(",", modifiers: .command)
    }
}
