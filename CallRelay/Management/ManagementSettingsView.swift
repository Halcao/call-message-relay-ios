import SwiftUI

/// Gateway parameters. Only the fields the operator actually edited are sent,
/// so an untouched value can never be overwritten by a stale parse.
struct ManagementSettingsView: View {
    @ObservedObject var center: AdminCenter
    @State private var overrides: [String: String] = [:]

    private var busy: Bool { center.pendingAction != nil }
    private var hasEdits: Bool { !overrides.isEmpty }

    var body: some View {
        Form {
            AdminFeedbackSection(center: center)
            if center.settings.isEmpty, center.isLoading(.settings) {
                Section {
                    HStack {
                        ProgressView()
                        Text("正在读取设置…").foregroundStyle(.secondary)
                    }
                }
            }
            Section {
                ForEach(SettingField.allCases) { field in
                    row(field)
                }
            } header: {
                Text("网关参数")
            } footer: {
                Text("仅保存你修改过的项目。范围：并发 1–32，宽限 5–600 秒，振铃 5–300 秒，离线 2–300 秒，留言最长 10–600 秒，保留 1–3650 天。")
            }
            if hasInvalidEdits {
                Section {
                    Text("请检查输入：需要数字且在给定范围内。")
                        .font(.caption)
                        .foregroundStyle(.red)
                }
            }
            Section {
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
                .disabled(busy || !hasEdits || hasInvalidEdits)
                .accessibilityIdentifier("managementSettingsSave")
                if hasEdits {
                    Button("放弃修改") { overrides = [:] }
                        .disabled(busy)
                }
            } footer: {
                if !hasEdits, !center.settings.isEmpty {
                    Text("没有未保存的修改。")
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("网关设置")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await center.refresh(.settings) }
        .task { await center.refresh(.settings) }
    }

    @ViewBuilder
    private func row(_ field: SettingField) -> some View {
        if field == .voicemailEnabled {
            Toggle(field.title, isOn: boolBinding(field))
                .disabled(busy)
        } else {
            HStack {
                Text(field.title)
                Spacer()
                TextField("", text: textBinding(field))
                    .keyboardType(.numberPad)
                    .multilineTextAlignment(.trailing)
                    .frame(maxWidth: 110)
                    .disabled(busy)
                if let unit = field.unit {
                    Text(unit).foregroundStyle(.secondary)
                }
            }
        }
    }

    private func value(_ field: SettingField) -> String {
        overrides[field.key] ?? center.settings[field.key] ?? ""
    }

    private func textBinding(_ field: SettingField) -> Binding<String> {
        Binding(
            get: { value(field) },
            set: { overrides[field.key] = $0 }
        )
    }

    private func boolBinding(_ field: SettingField) -> Binding<Bool> {
        Binding(
            get: { value(field) == "1" || value(field).lowercased() == "true" },
            set: { overrides[field.key] = $0 ? "1" : "0" }
        )
    }

    private var hasInvalidEdits: Bool {
        for (key, raw) in overrides {
            guard let field = SettingField.allCases.first(where: { $0.key == key }),
                  field != .voicemailEnabled else { continue }
            guard let number = Int(raw), number >= 0 else { return true }
            if let range = field.range, !range.contains(number) { return true }
        }
        return false
    }

    private func intValue(_ field: SettingField) -> Int? {
        guard let raw = overrides[field.key] else { return nil }
        return Int(raw)
    }

    private func boolValue(_ field: SettingField) -> Bool? {
        guard let raw = overrides[field.key] else { return nil }
        return raw == "1"
    }

    private func save() async {
        guard !hasInvalidEdits else { return }
        let saved = await center.updateSettings(
            maxConcurrentCalls: intValue(.maxConcurrentCalls),
            disconnectGraceSeconds: intValue(.disconnectGraceSeconds),
            voicemailEnabled: boolValue(.voicemailEnabled),
            voicemailRingSeconds: intValue(.voicemailRingSeconds),
            voicemailOfflineSeconds: intValue(.voicemailOfflineSeconds),
            voicemailMaxSeconds: intValue(.voicemailMaxSeconds),
            voicemailRetentionDays: intValue(.voicemailRetentionDays),
            voicemailMaxBytesMb: intValue(.voicemailMaxBytesMb),
            voicemailMinFreeBytesMb: intValue(.voicemailMinFreeBytesMb)
        )
        if saved {
            overrides = [:]
        }
    }
}

private enum SettingField: String, CaseIterable, Identifiable {
    case maxConcurrentCalls
    case disconnectGraceSeconds
    case voicemailEnabled
    case voicemailRingSeconds
    case voicemailOfflineSeconds
    case voicemailMaxSeconds
    case voicemailRetentionDays
    case voicemailMaxBytesMb
    case voicemailMinFreeBytesMb

    var id: String { rawValue }

    var key: String {
        switch self {
        case .maxConcurrentCalls: return "max_concurrent_calls"
        case .disconnectGraceSeconds: return "disconnect_grace_seconds"
        case .voicemailEnabled: return "voicemailEnabled"
        case .voicemailRingSeconds: return "voicemail_ring_seconds"
        case .voicemailOfflineSeconds: return "voicemail_offline_seconds"
        case .voicemailMaxSeconds: return "voicemail_max_seconds"
        case .voicemailRetentionDays: return "voicemail_retention_days"
        case .voicemailMaxBytesMb: return "voicemailMaxBytesMb"
        case .voicemailMinFreeBytesMb: return "voicemailMinFreeBytesMb"
        }
    }

    var title: String {
        switch self {
        case .maxConcurrentCalls: return "最大并发通话"
        case .disconnectGraceSeconds: return "通话结束宽限秒数"
        case .voicemailEnabled: return "语音留言开关"
        case .voicemailRingSeconds: return "留言振铃秒数"
        case .voicemailOfflineSeconds: return "离线转留言秒数"
        case .voicemailMaxSeconds: return "留言最长秒数"
        case .voicemailRetentionDays: return "留言保留天数"
        case .voicemailMaxBytesMb: return "留言容量上限"
        case .voicemailMinFreeBytesMb: return "留言最低剩余空间"
        }
    }

    var unit: String? {
        switch self {
        case .voicemailMaxBytesMb, .voicemailMinFreeBytesMb: return "MB"
        default: return nil
        }
    }

    var range: ClosedRange<Int>? {
        switch self {
        case .maxConcurrentCalls: return 1...32
        case .disconnectGraceSeconds: return 5...600
        case .voicemailRingSeconds: return 5...300
        case .voicemailOfflineSeconds: return 2...300
        case .voicemailMaxSeconds: return 10...600
        case .voicemailRetentionDays: return 1...3650
        default: return nil
        }
    }
}
