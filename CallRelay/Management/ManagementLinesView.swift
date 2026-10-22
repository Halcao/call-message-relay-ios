import SwiftUI

/// Gateway line inventory with a full per-line editor (enable/route toggles,
/// rejection policy, recording retention, and the line's own number).
struct ManagementLinesView: View {
    @ObservedObject var center: AdminCenter

    var body: some View {
        List {
            AdminFeedbackSection(center: center)
            if center.lines.isEmpty {
                Section {
                    if center.isLoading(.lines) {
                        HStack {
                            ProgressView()
                            Text("正在读取线路…").foregroundStyle(.secondary)
                        }
                    } else {
                        Text("未读取到线路，下拉可重试。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            ForEach(center.lines) { line in
                NavigationLink {
                    ManagementLineDetailView(center: center, lineId: line.id)
                } label: {
                    row(line)
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("线路")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await center.refresh(.lines) }
        .task { await center.refresh(.lines) }
    }

    private func row(_ line: AdminWire.Line) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(line.name).font(.body.weight(.medium))
                Spacer()
                Text(line.online ? "在线" : "离线")
                    .font(.caption)
                    .foregroundStyle(line.online ? AnyShapeStyle(.green) : AnyShapeStyle(.secondary))
            }
            Text(numberText(line))
                .font(.subheadline.monospacedDigit())
                .foregroundStyle(.secondary)
            Text("注册：\(AdminFormat.registrationLabel(line.registration)) · 短信实发：\(line.smsLive ? "开启" : "关闭") · 拦截：\(AdminFormat.rejectPolicyLabel(line.rejectPolicy))")
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
    }

    private func numberText(_ line: AdminWire.Line) -> String {
        if !line.numberLabel.isEmpty { return line.numberLabel }
        if !line.phoneNumber.isEmpty { return line.phoneNumber }
        return "未读取号码"
    }
}

struct ManagementLineDetailView: View {
    @ObservedObject var center: AdminCenter
    let lineId: String

    @State private var seeded = false
    @State private var name = ""
    @State private var enabled = true
    @State private var allowInbound = true
    @State private var allowOutbound = true
    @State private var smsLive = true
    @State private var rejectPolicy = "none"
    @State private var blockedPrefixes = ""
    @State private var retentionDays = 30
    @State private var manualNumber = ""
    @State private var showClearNumberConfirm = false

    private var line: AdminWire.Line? {
        center.lines.first { $0.id == lineId }
    }

    private var busy: Bool { center.pendingAction != nil }

    var body: some View {
        Form {
            AdminFeedbackSection(center: center)
            if let line {
                settingsSection(line)
                numberSection(line)
            } else if center.isLoading(.lines) {
                Section {
                    HStack {
                        ProgressView()
                        Text("正在读取线路…").foregroundStyle(.secondary)
                    }
                }
            } else {
                Section {
                    Text("线路不存在或已被移除。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle(line?.name ?? "线路设置")
        .navigationBarTitleDisplayMode(.inline)
        .task(id: line?.id) { seedIfNeeded() }
        .confirmationDialog("清除手动号码？", isPresented: $showClearNumberConfirm, titleVisibility: .visible) {
            Button("清除手动号码", role: .destructive) {
                Task { _ = await center.setLineNumber(id: lineId, manualNumber: "") }
            }
            Button("取消", role: .cancel) {}
        } message: {
            Text("将恢复为自动读取 SIM 卡号码。")
        }
    }

    @ViewBuilder
    private func settingsSection(_ line: AdminWire.Line) -> some View {
        Section {
            // Persistent leading labels: a Form TextField's placeholder
            // disappears as soon as a value exists, which left the editor
            // showing bare values (root review).
            LabeledContent("名称") {
                TextField("未命名", text: $name)
                    .multilineTextAlignment(.trailing)
            }
            Toggle("启用", isOn: $enabled)
            Toggle("允许呼入", isOn: $allowInbound)
            Toggle("允许呼出", isOn: $allowOutbound)
            // Truthful operational semantics: OFF means the worker keeps
            // messages in test/dry-run mode and never actually sends. It is a
            // real switch, so it stays visible — with the honest wording.
            Toggle("允许发送短信", isOn: $smsLive)
            Picker("拦截策略", selection: $rejectPolicy) {
                Text("不拦截").tag("none")
                Text("全部拒接").tag("reject_all")
                Text("拒接黑名单前缀").tag("reject_blocked")
            }
            LabeledContent("拦截前缀") {
                TextField("如 400,95", text: $blockedPrefixes)
                    .multilineTextAlignment(.trailing)
                    .autocorrectionDisabled()
            }
            Stepper(value: $retentionDays, in: 0...3650) {
                HStack {
                    Text("录音留存天数")
                    Spacer()
                    Text("\(retentionDays) 天").foregroundStyle(.secondary)
                }
            }
            Button {
                Task { await save() }
            } label: {
                HStack {
                    Text("保存")
                    if busy {
                        Spacer()
                        ProgressView()
                    }
                }
            }
            .disabled(busy)
            .accessibilityIdentifier("managementLinesSave")
        } header: {
            Text("线路设置")
        } footer: {
            Text("「允许发送短信」关闭时，网关只记录短信，不会真正发出。")
        }
    }

    @ViewBuilder
    private func numberSection(_ line: AdminWire.Line) -> some View {
        Section {
            HStack {
                Text("当前号码")
                Spacer()
                Text(numberText(line)).foregroundStyle(.secondary).multilineTextAlignment(.trailing)
            }
            HStack {
                Text("号码来源")
                Spacer()
                Text(AdminFormat.numberSourceLabel(line.numberSource)).foregroundStyle(.secondary)
            }
            TextField("手动号码", text: $manualNumber)
                .keyboardType(.phonePad)
            Button {
                Task {
                    let value = manualNumber.trimmingCharacters(in: .whitespacesAndNewlines)
                    _ = await center.setLineNumber(id: lineId, manualNumber: value)
                }
            } label: {
                HStack {
                    Text("保存号码")
                    if busy {
                        Spacer()
                        ProgressView()
                    }
                }
            }
            .disabled(busy || manualNumber.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            .accessibilityIdentifier("managementLineNumberSave")
            if line.numberSource == "manual" {
                Button("清除手动号码", role: .destructive) {
                    showClearNumberConfirm = true
                }
                .disabled(busy)
            }
            Button {
                Task { _ = await center.refreshLineIdentity(id: lineId) }
            } label: {
                HStack {
                    Text("重新读取号码")
                    if busy {
                        Spacer()
                        ProgressView()
                    }
                }
            }
            .disabled(busy)
            .accessibilityIdentifier("managementLineRefreshIdentity")
        } header: {
            Text("号码")
        } footer: {
            Text("手动号码保存在网关，供所有授权设备使用；清除后恢复自动读取。")
        }
    }

    private func numberText(_ line: AdminWire.Line) -> String {
        if !line.numberLabel.isEmpty { return line.numberLabel }
        if !line.phoneNumber.isEmpty { return line.phoneNumber }
        return "未读取"
    }

    private func seedIfNeeded() {
        guard !seeded, let line else { return }
        seeded = true
        name = line.name
        enabled = line.enabled
        allowInbound = line.allowInbound
        allowOutbound = line.allowOutbound
        smsLive = line.smsLive
        rejectPolicy = line.rejectPolicy
        blockedPrefixes = line.blockedPrefixes
        retentionDays = line.retentionDays
        manualNumber = line.numberSource == "manual" ? line.phoneNumber : ""
    }

    private func save() async {
        _ = await center.updateLine(
            id: lineId,
            name: name.trimmingCharacters(in: .whitespacesAndNewlines),
            enabled: enabled,
            allowInbound: allowInbound,
            allowOutbound: allowOutbound,
            rejectPolicy: rejectPolicy,
            blockedPrefixes: blockedPrefixes.trimmingCharacters(in: .whitespacesAndNewlines),
            retentionDays: retentionDays,
            smsLive: smsLive
        )
    }
}
