import AppKit
import Foundation
import SwiftUI

struct ServerEntry: Codable, Identifiable, Equatable {
    var id: String
    var name: String
    var url: String
    var hasKey: Bool
    var key: String?
    var title: String { name.isEmpty ? id : name }
}

struct Preferences: Codable, Equatable {
    var revision: String
    var exists: Bool
    var enabled: Bool
    var listen: String
    var mediaListen: String
    var models: [String: String]
    var gatewayUsage: Bool
    var usageOrganization: String
    var imageUpstream: String
    var servers: [ServerEntry]

    static let empty = Preferences(revision: "", exists: false, enabled: false,
        listen: "127.0.0.1:3426", mediaListen: "127.0.0.1:3425", models: [:],
        gatewayUsage: false, usageOrganization: "", imageUpstream: "", servers: [])
}

struct ModelMapping: Identifiable, Equatable {
    var id = UUID()
    var native: String
    var server: String
    var target: String
}

struct ServiceStatus: Decodable {
    var running: Bool
    var activeRequests: Int
    var version: String
    var listen: String
}

struct CertificateInfo: Decodable {
    var path: String
    var exists: Bool
    var trusted: Bool
    var sha256: String?
    var error: String?
}

struct ClientError: LocalizedError {
    var message: String
    var errorDescription: String? { message }
}

enum Helper {
    static let root = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".config/claude-login-proxy")
    static let configuration = root.appendingPathComponent("config.json")
    static var executable: URL {
        Bundle.main.resourceURL!.appendingPathComponent("claude-login-proxy")
    }
    static let service = "io.latentharbor.claude-login-proxy"
    static var domain: String { "gui/\(getuid())" }
    static var plist: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/LaunchAgents/\(service).plist")
    }

    // No shell is involved. Credentials travel over stdin, never process args.
    static func process(_ url: URL, _ arguments: [String], input: Data? = nil) async throws -> Data {
        try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                do {
                    let task = Process(), output = Pipe(), errors = Pipe(), source = Pipe()
                    task.executableURL = url
                    task.arguments = arguments
                    task.standardOutput = output
                    task.standardError = errors
                    task.standardInput = source
                    try task.run()
                    if let input { try source.fileHandleForWriting.write(contentsOf: input) }
                    try source.fileHandleForWriting.close()
                    let data = output.fileHandleForReading.readDataToEndOfFile()
                    let diagnostic = errors.fileHandleForReading.readDataToEndOfFile()
                    task.waitUntilExit()
                    guard task.terminationStatus == 0 else {
                        let message = String(data: diagnostic, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
                        throw ClientError(message: message?.isEmpty == false ? message! : "操作未完成，请检查运行日志。")
                    }
                    continuation.resume(returning: data)
                } catch { continuation.resume(throwing: error) }
            }
        }
    }

    static func call(_ command: String, input: Data? = nil) async throws -> Data {
        try await process(executable, [command, "--config", configuration.path], input: input)
    }
}

enum Page: String, CaseIterable, Identifiable {
    case overview = "运行状态", servers = "服务器", models = "模型映射", local = "本机与证书"
    var id: String { rawValue }
    var icon: String {
        switch self {
        case .overview: return "network"
        case .servers: return "externaldrive.connected.to.line.below"
        case .models: return "arrow.triangle.branch"
        case .local: return "lock.shield"
        }
    }
}

@MainActor final class ProxyModel: ObservableObject {
    @Published var preferences = Preferences.empty
    @Published var mappings: [ModelMapping] = []
    @Published var saved = Preferences.empty
    @Published var status: ServiceStatus?
    @Published var statusMessage = "正在读取服务状态"
    @Published var certificates: [String: CertificateInfo] = [:]
    @Published var catalogs: [String: [String]] = [:]
    @Published var catalogMessages: [String: String] = [:]
    @Published var message = ""
    @Published var isError = false
    @Published var busy = false
    @Published var loaded = false
    @Published var page: Page? = .overview
    @Published var logs = ""
    @Published var showLogs = false
    var preview = false

    var running: Bool { status?.running == true }
    var dirty: Bool { (try? document()) != saved }
    var installed: Bool { FileManager.default.fileExists(atPath: Helper.plist.path) }
    var active: Int { status?.activeRequests ?? 0 }
    var canStop: Bool { running && active == 0 && !busy }

    func document() throws -> Preferences {
        var value = preferences
        value.models = [:]
        for m in mappings {
            let native = m.native.trimmingCharacters(in: .whitespacesAndNewlines)
            let target = m.target.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !native.isEmpty, !target.isEmpty, !m.server.isEmpty else {
                throw ClientError(message: "每条模型映射都需要 Desktop 模型、服务器和目标模型。")
            }
            guard value.models[native] == nil else {
                throw ClientError(message: "Desktop 模型 \(native) 重复了，请保留一条映射。")
            }
            value.models[native] = m.server + "/" + target
        }
        return value
    }

    func apply(_ p: Preferences) {
        preferences = p
        saved = p
        mappings = p.models.sorted { $0.key < $1.key }.map { native, target in
            let parts = target.split(separator: "/", maxSplits: 1).map(String.init)
            return ModelMapping(native: native, server: parts.first ?? "", target: parts.count == 2 ? parts[1] : "")
        }
        loaded = true
    }

    func report(_ text: String, error: Bool = false) { message = text; isError = error }

    func load() async {
        guard !busy else { return }
        busy = true
        defer { busy = false }
        do {
            let p = try JSONDecoder().decode(Preferences.self, from: await Helper.call("preferences"))
            apply(p)
            if !p.exists { page = .servers; report("添加服务器，检查连接并配置模型后保存。") }
        } catch { report(error.localizedDescription, error: true) }
        await refreshStatus()
        await refreshCertificates()
    }

    func save() async {
        guard !busy else { return }
        busy = true
        defer { busy = false }
        do {
            let data = try JSONEncoder().encode(document())
            let updated = try JSONDecoder().decode(Preferences.self, from: await Helper.call("save-preferences", input: data))
            apply(updated)
            report(running ? "已保存，后续请求使用新配置。" : "已保存。确认本机证书后即可启动服务。")
            await refreshCertificates()
        } catch { report(error.localizedDescription, error: true) }
    }

    func refreshStatus() async {
        if preview { return }
        do {
            status = try JSONDecoder().decode(ServiceStatus.self, from: await Helper.call("status"))
            statusMessage = running ? "后台代理正在运行" : "后台服务正在启动"
        } catch {
            status = nil
            statusMessage = "后台服务未运行或无法读取状态"
        }
    }

    func refreshCertificates() async {
        if preview || !preferences.exists { return }
        do {
            certificates = try JSONDecoder().decode([String: CertificateInfo].self, from: await Helper.call("certificates"))
        } catch { report(error.localizedDescription, error: true) }
    }

    func addServer() {
        preferences.servers.append(ServerEntry(id: "gateway-" + UUID().uuidString.prefix(8).lowercased(), name: "新服务器", url: "", hasKey: false, key: ""))
        page = .servers
    }

    func removeServer(_ id: String) {
        if mappings.contains(where: { $0.server == id }) || preferences.imageUpstream == id {
            report("先移除该服务器的模型映射，并更换图像服务，再删除服务器。", error: true)
            return
        }
        preferences.servers.removeAll { $0.id == id }
        catalogs[id] = nil
    }

    func probe(_ id: String) async {
        guard !busy, let server = preferences.servers.first(where: { $0.id == id }) else { return }
        busy = true
        defer { busy = false }
        catalogMessages[id] = "正在检查连接…"
        do {
            let input = try JSONEncoder().encode(server)
            let models = try JSONDecoder().decode([String].self, from: await Helper.call("probe-server", input: input))
            catalogs[id] = models
            catalogMessages[id] = "连接正常 · \(models.count) 个模型"
            report("已读取模型目录，不会产生推理请求。")
        } catch {
            catalogs[id] = nil
            catalogMessages[id] = "连接失败"
            report(error.localizedDescription, error: true)
        }
    }

    func addMapping() {
        mappings.append(ModelMapping(native: "", server: preferences.servers.first?.id ?? "", target: ""))
        page = .models
    }

    func service(_ action: String) async {
        guard !busy else { return }
        busy = true
        defer { busy = false }
        do {
            if action == "stop" {
                let fresh = try JSONDecoder().decode(ServiceStatus.self, from: await Helper.call("status"))
                guard fresh.running, fresh.activeRequests == 0 else {
                    throw ClientError(message: "仍有请求正在进行，请结束后再停止服务。")
                }
                _ = try await Helper.process(URL(fileURLWithPath: "/bin/launchctl"), ["bootout", Helper.domain + "/" + Helper.service])
                report("服务已停止；Claude 的代理连接暂不可用。")
            } else if action == "install" {
                let script = Bundle.main.resourceURL!.appendingPathComponent("install.py")
                _ = try await Helper.process(URL(fileURLWithPath: "/usr/bin/python3"), [script.path, "--upgrade"])
                report("后台服务已安装，登录后自动启动。")
            } else {
                _ = try await Helper.process(URL(fileURLWithPath: "/bin/launchctl"), ["bootstrap", Helper.domain, Helper.plist.path])
                report("已启动后台服务。")
            }
            // Give launchd time to bind the status socket; bounded and nonblocking.
            for _ in 0..<15 {
                await refreshStatus()
                if action == "stop" || running { break }
                try await Task.sleep(nanoseconds: 200_000_000)
            }
            if action != "stop" && !running { report("服务尚未就绪，请检查证书信任和运行日志。", error: true) }
        } catch { report(error.localizedDescription, error: true) }
    }

    func reveal(_ url: URL) { NSWorkspace.shared.activateFileViewerSelecting([url]) }

    func readLogs() {
        let root = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Logs/Claude Login Proxy")
        logs = ["error.log", "service.log"].map { name in
            guard let f = try? FileHandle(forReadingFrom: root.appendingPathComponent(name)) else { return "\(name)\n暂无日志" }
            defer { try? f.close() }
            let size = (try? f.seekToEnd()) ?? 0
            try? f.seek(toOffset: size > 32768 ? size - 32768 : 0)
            let data = (try? f.readToEnd()) ?? Data()
            return name + "\n" + (String(data: data, encoding: .utf8) ?? "无法读取日志")
        }.joined(separator: "\n\n")
        showLogs = true
    }
}
