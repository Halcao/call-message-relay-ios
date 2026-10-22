import SwiftUI

/// Recovery and diagnostics: per-line recovery probes, push/relay state,
/// recent events and the administrator action log.
struct ManagementDiagnosticsView: View {
    @ObservedObject var center: AdminCenter

    private var diagnostics: AdminWire.Diagnostics? { center.diagnostics }
    private var busy: Bool { center.pendingAction != nil }

    var body: some View {
        List {
            AdminFeedbackSection(center: center)
            Section("线路") {
                if let lines = diagnostics?.lines, !lines.isEmpty {
                    ForEach(lines) { line in
                        HStack {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(line.name)
                                Text("\(line.online ? "在线" : "离线") · 注册：\(AdminFormat.registrationLabel(line.registration))")
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                            }
                            Spacer()
                            Button("恢复探测") {
                                Task { _ = await center.recoverLine(id: line.id) }
                            }
                            .font(.caption)
                            .disabled(busy)
                            .accessibilityIdentifier("managementRecoverLine")
                        }
                    }
                } else if center.isLoading(.diagnostics) {
                    HStack {
                        ProgressView()
                        Text("正在读取…").foregroundStyle(.secondary)
                    }
                } else {
                    Text("没有线路信息。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }

            Section("推送与中继") {
                if let diagnostics {
                    statusRow("推送就绪", diagnostics.pushReady ? "是" : "否")
                    statusRow("中继策略", relayPolicyText(diagnostics.turnPolicy))
                } else {
                    Text("尚未读取诊断信息，下拉可重试。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }

            Section("近期事件") {
                if let events = diagnostics?.events, !events.isEmpty {
                    ForEach(Array(events.enumerated()), id: \.offset) { _, event in
                        VStack(alignment: .leading, spacing: 2) {
                            Text(event.summary ?? event.kind ?? "事件")
                            Text(AdminFormat.time(event.at))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                } else {
                    Text("没有近期事件。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }

            Section("操作记录") {
                if let audit = diagnostics?.audit, !audit.isEmpty {
                    ForEach(audit) { entry in
                        VStack(alignment: .leading, spacing: 2) {
                            Text("\(entry.actor) · \(entry.action)")
                            Text("目标：\(entry.target) · 结果：\(entry.result) · \(AdminFormat.time(entry.at))")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                } else {
                    Text("没有操作记录。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("诊断与恢复")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await center.refresh(.diagnostics) }
        .task { await center.refresh(.diagnostics) }
    }

    private func statusRow(_ title: String, _ value: String) -> some View {
        HStack {
            Text(title)
            Spacer()
            Text(value).foregroundStyle(.secondary)
        }
    }

    private func relayPolicyText(_ policy: String?) -> String {
        guard let policy, !policy.isEmpty else { return "未知" }
        switch policy {
        case "auto": return "自动"
        case "relay", "turn", "server": return "经网关中继"
        case "direct", "p2p": return "直连"
        case "off", "disabled": return "关闭"
        default: return "未知"
        }
    }
}
