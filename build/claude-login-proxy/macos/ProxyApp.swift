import AppKit
import SwiftUI

@main @MainActor struct ClaudeProxyApp: App {
    @StateObject private var model = ProxyModel()
    var body: some Scene {
        Window("Claude Login Proxy", id: "settings") {
            ProxyWindow(model: model)
                .task { if !model.loaded { await model.load() } }
        }
        .defaultSize(width: 1000, height: 720)
        .commands {
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
            openWindow(id: "settings")
            NSApplication.shared.activate(ignoringOtherApps: true)
        }.keyboardShortcut(",", modifiers: .command)
    }
}
