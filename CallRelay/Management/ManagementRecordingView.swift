import SwiftUI

/// Gateway-wide recording policy (not the per-device auto preference, which
/// stays in 设置 → 通话录音).
struct ManagementRecordingView: View {
    @ObservedObject var center: AdminCenter

    private var busy: Bool { center.pendingAction != nil }

    var body: some View {
        Form {
            AdminFeedbackSection(center: center)
            if let policy = center.recordingPolicy {
                if !policy.configured {
                    Section {
                        Text("当前网关未启用录音存储，录音开关与自动录音不会生效。")
                            .font(.caption)
                            .foregroundStyle(.orange)
                    }
                }
                Section {
                    Toggle("录音功能", isOn: Binding(
                        get: { policy.enabled },
                        set: { value in
                            Task { _ = await center.setRecording(enabled: value, autoMode: nil) }
                        }
                    ))
                    .disabled(!policy.configured || busy)
                    Picker("自动录音模式", selection: Binding(
                        get: { policy.autoMode },
                        set: { value in
                            Task { _ = await center.setRecording(enabled: nil, autoMode: value) }
                        }
                    )) {
                        Text("关闭").tag("off")
                        Text("全部通话").tag("all")
                        Text("仅呼入").tag("incoming")
                        Text("仅呼出").tag("outgoing")
                    }
                    .pickerStyle(.menu)
                    .disabled(!policy.configured || busy)
                    .accessibilityIdentifier("managementRecordingToggle")
                } footer: {
                    Text("本机自动录音偏好请在「设置 → 通话录音」中调整；此处是网关全局策略，两者同时开启才会自动录音。")
                }
                Section("存储") {
                    HStack {
                        Text("保留天数")
                        Spacer()
                        Text(policy.retentionDays > 0 ? "\(policy.retentionDays) 天" : "手动删除")
                            .foregroundStyle(.secondary)
                    }
                    HStack {
                        Text("存储状态")
                        Spacer()
                        Text(policy.storageAvailable ? "可用" : "不可用")
                            .foregroundStyle(policy.storageAvailable ? AnyShapeStyle(.secondary) : AnyShapeStyle(.orange))
                    }
                    if let free = policy.freeBytes {
                        HStack {
                            Text("剩余空间")
                            Spacer()
                            Text(AdminFormat.bytes(free)).foregroundStyle(.secondary)
                        }
                    }
                    if let used = policy.usedBytes {
                        HStack {
                            Text("已用空间")
                            Spacer()
                            Text(AdminFormat.bytes(used)).foregroundStyle(.secondary)
                        }
                    }
                    if policy.maxBytes > 0 {
                        HStack {
                            Text("容量上限")
                            Spacer()
                            Text(AdminFormat.bytes(policy.maxBytes)).foregroundStyle(.secondary)
                        }
                    }
                    if policy.minimumFreeBytes > 0 {
                        HStack {
                            Text("最低剩余空间")
                            Spacer()
                            Text(AdminFormat.bytes(policy.minimumFreeBytes)).foregroundStyle(.secondary)
                        }
                    }
                    if let error = policy.storageError, !error.isEmpty {
                        Text(error)
                            .font(.caption)
                            .foregroundStyle(.orange)
                    }
                }
            } else if center.isLoading(.recording) {
                Section {
                    HStack {
                        ProgressView()
                        Text("正在读取录音策略…").foregroundStyle(.secondary)
                    }
                }
            } else {
                Section {
                    Text("尚未读取录音策略，下拉可重试。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("录音策略")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await center.refresh(.recording) }
        .task { await center.refresh(.recording) }
    }
}
