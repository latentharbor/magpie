import AppKit
import SwiftUI

struct ProxyWindow: View {
    @ObservedObject var model: ProxyModel
    @State private var confirmReload = false
    @State private var confirmStop = false

    var body: some View {
        NavigationSplitView {
            List(Page.allCases, selection: $model.page) { page in
                Label(page.rawValue, systemImage: page.icon).tag(page)
            }
            .navigationSplitViewColumnWidth(min: 175, ideal: 185, max: 230)
            .safeAreaInset(edge: .bottom) {
                Label(model.running ? "代理运行中" : "代理未运行", systemImage: model.running ? "checkmark.circle.fill" : "pause.circle")
                    .font(.caption).foregroundStyle(.secondary).padding()
            }
        } detail: {
            VStack(spacing: 0) {
                if !model.message.isEmpty {
                    HStack(alignment: .top, spacing: 10) {
                        Image(systemName: model.isError ? "exclamationmark.triangle" : "checkmark.circle")
                            .foregroundStyle(model.isError ? Color.orange : Color.secondary)
                        Text(model.message).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                        Button { model.message = "" } label: { Image(systemName: "xmark") }
                            .buttonStyle(.plain).accessibilityLabel("关闭提示")
                    }
                    .padding(14).background(.quaternary.opacity(0.5))
                    Divider()
                }
                switch model.page ?? .overview {
                case .overview: overview
                case .servers: servers
                case .models: models
                case .local: local
                }
            }
            .navigationTitle(model.page?.rawValue ?? "Claude Login Proxy")
            .toolbar {
                ToolbarItemGroup {
                    if model.busy { ProgressView().controlSize(.small).accessibilityLabel("正在处理") }
                    Button("重新载入", systemImage: "arrow.clockwise") {
                        if model.dirty { confirmReload = true } else { Task { await model.load() } }
                    }.disabled(model.busy)
                    Button("保存配置") { Task { await model.save() } }
                        .keyboardShortcut("s", modifiers: .command)
                        .disabled(!model.loaded || !model.dirty || model.busy)
                        .buttonStyle(.borderedProminent)
                }
            }
        }
        .frame(minWidth: 850, minHeight: 590)
        .confirmationDialog("放弃未保存的修改并重新载入？", isPresented: $confirmReload) {
            Button("放弃修改", role: .destructive) { Task { await model.load() } }
        }
        .confirmationDialog("停止后台代理？", isPresented: $confirmStop) {
            Button("停止代理", role: .destructive) { Task { await model.service("stop") } }
        } message: { Text("Claude Desktop 的代理连接会暂时不可用，重新启动服务即可恢复。") }
        .sheet(isPresented: $model.showLogs) {
            VStack(alignment: .leading, spacing: 14) {
                HStack { Text("运行日志").font(.headline); Spacer(); Button("关闭") { model.showLogs = false }.keyboardShortcut(.cancelAction) }
                ScrollView([.horizontal, .vertical]) {
                    Text(model.logs).font(.system(.caption, design: .monospaced)).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                }
            }.padding(20).frame(width: 760, height: 480)
        }
    }

    private var overview: some View {
        Form {
            Section {
                HStack(alignment: .top, spacing: 16) {
                    Image(systemName: model.running ? "network" : "pause.circle")
                        .font(.system(size: 34, weight: .light)).foregroundStyle(.tint)
                        .accessibilityHidden(true)
                    VStack(alignment: .leading, spacing: 6) {
                        Text(model.statusMessage).font(.title3.weight(.semibold))
                        Text("本机连接 Claude，远程网关处理模型请求与图像生成。")
                            .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }.padding(.vertical, 6)
                LabeledContent("进行中的请求", value: model.status.map { String($0.activeRequests) } ?? "未知")
                LabeledContent("后台版本", value: model.status?.version ?? "未运行")
                HStack {
                    Button("刷新状态") { Task { await model.refreshStatus(); await model.refreshCertificates() } }
                    if model.running {
                        Button("停止服务", role: .destructive) { confirmStop = true }.disabled(!model.canStop)
                    } else if model.installed {
                        Button("启动服务") { Task { await model.service("start") } }.disabled(model.busy)
                    }
                    Button(model.installed ? "更新后台组件" : "安装并接入 Claude") { Task { await model.service("install") } }
                        .disabled(model.busy || model.dirty || !model.preferences.exists || model.active > 0)
                }
            } header: { Text("服务") } footer: {
                Text("关闭此窗口或退出设置应用，后台代理仍会运行。服务安装后随用户登录启动。")
            }
            LoginSettings()
            Section("当前配置") {
                LabeledContent("服务器", value: "\(model.preferences.servers.count) 个")
                LabeledContent("模型映射", value: "\(model.mappings.count) 条")
                LabeledContent("HTTPS 代理", value: model.preferences.listen)
                LabeledContent("图像工具转发", value: model.preferences.imageUpstream.isEmpty ? "关闭" : serverTitle(model.preferences.imageUpstream))
            }
            Section {
                HStack {
                    Button("查看日志") { model.readLogs() }
                    Button("打开配置目录") { model.reveal(Helper.configuration) }
                    Button("打开备份") { model.reveal(Helper.root.appendingPathComponent("backups")) }
                }
            } footer: {
                Text("订阅权限和真实额度由远程网关与官方账户管理。本应用不保存订阅账户登录凭证。")
            }
        }.formStyle(.grouped)
    }

    private var servers: some View {
        Form {
            Section {
                Text("填写 Magpie 或兼容 Anthropic Messages 的网关地址。使用独立的设备密钥。")
                    .foregroundStyle(.secondary)
                Button("添加服务器", systemImage: "plus") { model.addServer() }.disabled(model.busy)
            }
            ForEach($model.preferences.servers) { $server in
                Section {
                    TextField("名称", text: $server.name, prompt: Text("家里的 NAS"))
                    TextField("API 地址", text: $server.url, prompt: Text("http://192.168.1.10:3425"))
                        .textContentType(.URL)
                    SecureField(server.hasKey ? "新密钥（留空保留）" : "API 密钥", text: Binding(get: { server.key ?? "" }, set: { server.key = $0 }))
                    HStack {
                        Button("检查连接并读取模型") { Task { await model.probe(server.id) } }.disabled(model.busy || server.url.isEmpty)
                        if let text = model.catalogMessages[server.id] { Text(text).font(.caption).foregroundStyle(.secondary) }
                        Spacer()
                        Button("移除", role: .destructive) { model.removeServer(server.id) }.disabled(model.busy)
                    }
                } header: { Text(server.title) } footer: {
                    Text("地址只填写协议、主机与端口，不要附加 /v1。密钥仅存于本机权限为 0600 的独立文件。")
                }
            }
        }.formStyle(.grouped).disabled(model.busy)
    }

    private var models: some View {
        Form {
            Section {
                Toggle("通过网关转发已映射的模型", isOn: $model.preferences.enabled)
                Text("左侧填写 Claude Desktop 请求的模型 ID，右侧选择远程目标。没有映射的请求按官方路径处理；已映射请求失败时不会回退到官方推理。")
                    .foregroundStyle(.secondary)
                HStack {
                    Button("添加映射", systemImage: "plus") { model.addMapping() }.disabled(model.preferences.servers.isEmpty || model.busy)
                    Spacer()
                    Text("\(model.mappings.count) 条映射").font(.caption).foregroundStyle(.secondary)
                }
            }
            ForEach($model.mappings) { $mapping in
                Section {
                    TextField("Desktop 模型 ID", text: $mapping.native, prompt: Text("claude-opus-5-5"))
                    Picker("服务器", selection: $mapping.server) {
                        Text("请选择").tag("")
                        ForEach(model.preferences.servers) { s in Text(s.title).tag(s.id) }
                    }
                    HStack {
                        TextField("目标模型", text: $mapping.target, prompt: Text("provider/model"))
                        Menu {
                            if let options = model.catalogs[mapping.server], !options.isEmpty {
                                ForEach(options, id: \.self) { name in Button(name) { mapping.target = name } }
                            } else {
                                Button("先读取该服务器的模型目录") { Task { await model.probe(mapping.server) } }
                            }
                        } label: { Image(systemName: "list.bullet") }.accessibilityLabel("选择目标模型")
                    }
                    HStack {
                        Text("按完整 ID 匹配").font(.caption).foregroundStyle(.secondary)
                        Spacer()
                        Button("移除映射", role: .destructive) { model.mappings.removeAll { $0.id == mapping.id } }
                    }
                }
            }
        }.formStyle(.grouped).disabled(model.busy)
    }

    private var local: some View {
        Form {
            Section {
                Toggle("由网关管理本地 Code 额度显示", isOn: $model.preferences.gatewayUsage)
                TextField("Desktop 组织 UUID", text: $model.preferences.usageOrganization, prompt: Text("组织 UUID，可留空"))
                Text("仅处理已配置组织的额度显示，真实用量仍由远程网关管理。订阅、登录和账户验证检查保持官方行为。")
                    .foregroundStyle(.secondary)
            } header: { Text("额度显示") }
            Section {
                Picker("图像服务", selection: $model.preferences.imageUpstream) {
                    Text("关闭").tag("")
                    ForEach(model.preferences.servers) { s in Text(s.title).tag(s.id) }
                }
                Text("图像工具需要 Magpie 的 image MCP 接口。本机文件仅在工具调用指定时上传，生成结果保存到本机。")
                    .foregroundStyle(.secondary)
            } header: { Text("图像工具") }
            Section {
                ForEach(model.certificates.keys.sorted(), id: \.self) { key in
                    if let cert = model.certificates[key] {
                        VStack(alignment: .leading, spacing: 8) {
                            HStack {
                                Label(key == "api" ? "api.anthropic.com" : "claude.ai", systemImage: cert.trusted ? "checkmark.shield" : "exclamationmark.shield")
                                Spacer()
                                Text(cert.trusted ? "已信任" : "需要信任").foregroundStyle(.secondary)
                            }
                            if let hash = cert.sha256 {
                                Text("SHA-256: " + hash).font(.system(.caption, design: .monospaced))
                                    .textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                            }
                            if let error = cert.error { Text(error).font(.caption).foregroundStyle(.orange) }
                            HStack {
                                Button("在 Finder 中显示") { model.reveal(URL(fileURLWithPath: cert.path)) }
                                Button("用钥匙串打开公开证书") { NSWorkspace.shared.open(URL(fileURLWithPath: cert.path)) }.disabled(!cert.exists)
                            }
                        }.padding(.vertical, 5)
                    }
                }
                Button("刷新证书状态") { Task { await model.refreshCertificates() } }.disabled(model.busy)
            } header: { Text("本机证书") } footer: {
                Text("证书信任需要你在钥匙串中确认；应用不会自动添加信任。私钥只保留在本机。")
            }
            Section("本地端口") {
                LabeledContent("HTTPS 代理", value: model.preferences.listen)
                LabeledContent("图像兼容转发", value: model.preferences.mediaListen)
                Text("当前端口已被 Desktop 与旧图像工具引用，界面保留这些端口以避免断开现有连接。")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }.formStyle(.grouped).disabled(model.busy)
    }

    private func serverTitle(_ id: String) -> String {
        model.preferences.servers.first { $0.id == id }?.title ?? id
    }
}
