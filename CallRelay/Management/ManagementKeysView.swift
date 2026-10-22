import SwiftUI
import UIKit

/// Pairing keys grouped by state (有效 / 已撤销) with create/reveal flows and
/// a per-key line-grant editor.
struct ManagementKeysView: View {
    @ObservedObject var center: AdminCenter
    @State private var showCreate = false
    @State private var material: PresentedKeyMaterial?
    @State private var revokeCandidate: AdminWire.Key?
    @State private var deleteCandidate: AdminWire.Key?

    private var activeKeys: [AdminWire.Key] { center.keys.filter { !$0.revoked } }
    private var revokedKeys: [AdminWire.Key] { center.keys.filter { $0.revoked } }
    private var busy: Bool { center.pendingAction != nil }

    var body: some View {
        List {
            AdminFeedbackSection(center: center)
            if center.keys.isEmpty {
                Section {
                    if center.isLoading(.keys) {
                        HStack {
                            ProgressView()
                            Text("正在读取密钥…").foregroundStyle(.secondary)
                        }
                    } else {
                        Text("还没有配对密钥，点按“新建密钥”创建。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            if !activeKeys.isEmpty {
                Section("有效") {
                    ForEach(activeKeys) { key in keyRow(key) }
                }
            }
            if !revokedKeys.isEmpty {
                Section("已撤销") {
                    ForEach(revokedKeys) { key in keyRow(key) }
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("配对密钥")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    showCreate = true
                } label: {
                    Label("新建密钥", systemImage: "plus")
                }
                .disabled(busy)
                .accessibilityIdentifier("managementKeyCreate")
            }
        }
        .refreshable { await center.refresh(.keys) }
        .task { await center.refresh(.keys) }
        .sheet(isPresented: $showCreate) {
            KeyCreateSheet(center: center)
        }
        .sheet(item: $material) { item in
            KeyMaterialSheet(material: item.material)
        }
        .confirmationDialog(
            "撤销这个密钥？",
            isPresented: Binding(
                get: { revokeCandidate != nil },
                set: { if !$0 { revokeCandidate = nil } }
            ),
            titleVisibility: .visible,
            presenting: revokeCandidate
        ) { key in
            Button("撤销", role: .destructive) {
                Task { _ = await center.revokeKey(id: key.id) }
            }
            Button("取消", role: .cancel) {}
        } message: { _ in
            Text("关联设备与新注册将立即失效。")
        }
        .confirmationDialog(
            "删除这个密钥？",
            isPresented: Binding(
                get: { deleteCandidate != nil },
                set: { if !$0 { deleteCandidate = nil } }
            ),
            titleVisibility: .visible,
            presenting: deleteCandidate
        ) { key in
            Button("删除", role: .destructive) {
                Task { _ = await center.deleteKey(id: key.id) }
            }
            Button("取消", role: .cancel) {}
        } message: { _ in
            Text("仅已撤销的密钥可以删除，删除后线路授权一并移除。")
        }
    }

    @ViewBuilder
    private func keyRow(_ key: AdminWire.Key) -> some View {
        NavigationLink {
            ManagementKeyGrantsView(center: center, keyId: key.id)
        } label: {
            VStack(alignment: .leading, spacing: 4) {
                HStack {
                    Text(key.name)
                    Spacer()
                    if key.manageNumber {
                        Text("号码管理")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 6)
                            .padding(.vertical, 2)
                            .background(Color(.secondarySystemFill), in: Capsule())
                    }
                }
                Text(keySummary(key))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if !key.revoked, !key.secretRecoverable {
                    Text("该密钥创建时只保存了校验值，无法恢复显示；请新建密钥替换。")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
        }
        .swipeActions(edge: .trailing, allowsFullSwipe: false) {
            swipeActions(for: key)
        }
        .contextMenu {
            contextActions(for: key)
        }
    }

    @ViewBuilder
    private func swipeActions(for key: AdminWire.Key) -> some View {
        if key.revoked {
            Button(role: .destructive) {
                deleteCandidate = key
            } label: {
                Label("删除", systemImage: "trash")
            }
        } else {
            if key.secretRecoverable {
                Button {
                    Task { await reveal(key) }
                } label: {
                    Label("显示密钥", systemImage: "eye")
                }
                .tint(.blue)
                .accessibilityIdentifier("managementKeyReveal")
            }
            Button(role: .destructive) {
                revokeCandidate = key
            } label: {
                Label("撤销", systemImage: "xmark.circle")
            }
        }
    }

    @ViewBuilder
    private func contextActions(for key: AdminWire.Key) -> some View {
        if key.revoked {
            Button(role: .destructive) {
                deleteCandidate = key
            } label: {
                Label("删除", systemImage: "trash")
            }
        } else {
            if key.secretRecoverable {
                Button {
                    Task { await reveal(key) }
                } label: {
                    Label("显示密钥", systemImage: "eye")
                }
            }
            Button(role: .destructive) {
                revokeCandidate = key
            } label: {
                Label("撤销", systemImage: "xmark.circle")
            }
        }
    }

    private func keySummary(_ key: AdminWire.Key) -> String {
        var parts: [String] = []
        parts.append("创建于 \(AdminFormat.time(key.createdAt))")
        parts.append(key.allLines ? "全部线路" : "\(key.grants.count) 条线路授权")
        if key.revoked {
            if let revokedAt = key.revokedAt, revokedAt > 0 {
                parts.append("撤销于 \(AdminFormat.time(revokedAt))")
            } else {
                parts.append("已撤销")
            }
        }
        return parts.joined(separator: " · ")
    }

    private func reveal(_ key: AdminWire.Key) async {
        guard center.pendingAction == nil else { return }
        if let revealed = await center.revealKey(id: key.id) {
            material = PresentedKeyMaterial(material: revealed)
        }
    }
}

struct ManagementKeyGrantsView: View {
    @ObservedObject var center: AdminCenter
    let keyId: String

    @State private var deleteGrantCandidate: AdminWire.Grant?

    private enum GrantField {
        case receiveSms, receiveCalls, sendSms, dial
    }

    private var key: AdminWire.Key? {
        center.keys.first { $0.id == keyId }
    }

    private var busy: Bool { center.pendingAction != nil }

    private var linesWithoutGrant: [AdminWire.Line] {
        guard let key else { return [] }
        let granted = Set(key.grants.map(\.lineId))
        return center.lines.filter { !granted.contains($0.id) }
    }

    var body: some View {
        Form {
            AdminFeedbackSection(center: center)
            if let key {
                Section {
                    Toggle("号码管理授权", isOn: Binding(
                        get: { key.manageNumber },
                        set: { value in
                            Task { _ = await center.setKeyManageNumber(id: key.id, allowed: value) }
                        }
                    ))
                    .disabled(busy || key.revoked)
                } header: {
                    Text("密钥")
                } footer: {
                    Text("开启后，使用该密钥的设备可以修改线路自己的号码。")
                }

                if key.revoked {
                    Section {
                        Text("该密钥已撤销，授权不可再修改。")
                            .font(.caption)
                            .foregroundStyle(.orange)
                    }
                    ForEach(key.grants) { grant in
                        Section {
                            Text(grantSummary(grant))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        } header: {
                            Text(grant.lineName)
                        }
                    }
                } else {
                    if key.grants.isEmpty {
                        Section("线路授权") {
                            Text("尚未授权任何线路。")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                    ForEach(key.grants) { grant in
                        Section {
                            grantToggle("接收短信", field: .receiveSms, grant: grant)
                            grantToggle("接收来电", field: .receiveCalls, grant: grant)
                            grantToggle("发送短信", field: .sendSms, grant: grant)
                            grantToggle("拨号", field: .dial, grant: grant)
                            Button("移除授权", role: .destructive) {
                                deleteGrantCandidate = grant
                            }
                            .disabled(busy)
                        } header: {
                            Text(grant.lineName)
                        }
                    }
                    if !linesWithoutGrant.isEmpty {
                        Section {
                            Menu {
                                ForEach(linesWithoutGrant) { line in
                                    Button(line.name) {
                                        Task {
                                            _ = await center.setGrant(
                                                keyId: key.id, lineId: line.id,
                                                receiveSms: true, receiveCalls: true,
                                                sendSms: false, dial: false
                                            )
                                        }
                                    }
                                }
                            } label: {
                                Label("添加授权", systemImage: "plus.circle")
                            }
                            .disabled(busy)
                        } footer: {
                            Text("新增授权默认允许接收短信与来电，不包含发送与拨号。")
                        }
                    }
                }
            } else {
                Section {
                    Text("该密钥不存在或已被删除。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle(key?.name ?? "密钥授权")
        .navigationBarTitleDisplayMode(.inline)
        .task { await center.refresh(.keys) }
        .confirmationDialog(
            "移除这条线路授权？",
            isPresented: Binding(
                get: { deleteGrantCandidate != nil },
                set: { if !$0 { deleteGrantCandidate = nil } }
            ),
            titleVisibility: .visible,
            presenting: deleteGrantCandidate
        ) { grant in
            Button("移除授权", role: .destructive) {
                Task { _ = await center.deleteGrant(keyId: keyId, lineId: grant.lineId) }
            }
            Button("取消", role: .cancel) {}
        } message: { grant in
            Text("使用该密钥的设备将不能再使用“\(grant.lineName)”。")
        }
    }

    private func grantSummary(_ grant: AdminWire.Grant) -> String {
        let parts = [
            "接收短信：\(grant.receiveSms ? "是" : "否")",
            "接收来电：\(grant.receiveCalls ? "是" : "否")",
            "发送短信：\(grant.sendSms ? "是" : "否")",
            "拨号：\(grant.dial ? "是" : "否")",
        ]
        return parts.joined(separator: " · ")
    }

    private func grantToggle(_ title: String, field: GrantField, grant: AdminWire.Grant) -> some View {
        Toggle(title, isOn: Binding(
            get: {
                switch field {
                case .receiveSms: return grant.receiveSms
                case .receiveCalls: return grant.receiveCalls
                case .sendSms: return grant.sendSms
                case .dial: return grant.dial
                }
            },
            set: { newValue in
                Task {
                    _ = await center.setGrant(
                        keyId: keyId,
                        lineId: grant.lineId,
                        receiveSms: field == .receiveSms ? newValue : grant.receiveSms,
                        receiveCalls: field == .receiveCalls ? newValue : grant.receiveCalls,
                        sendSms: field == .sendSms ? newValue : grant.sendSms,
                        dial: field == .dial ? newValue : grant.dial
                    )
                }
            }
        ))
        .disabled(busy)
    }
}

struct KeyCreateSheet: View {
    @ObservedObject var center: AdminCenter
    @Environment(\.dismiss) private var dismiss

    @State private var name = ""
    @State private var allLines = false
    @State private var manageNumber = false
    @State private var material: PresentedKeyMaterial?

    var body: some View {
        NavigationStack {
            Form {
                // A failed create must explain itself inside THIS sheet.
                AdminFeedbackSection(center: center)
                Section {
                    TextField("密钥名称", text: $name)
                    Toggle("全部线路", isOn: $allLines)
                    Toggle("允许号码管理", isOn: $manageNumber)
                } footer: {
                    Text("创建后会显示一次完整密钥，请立即复制或扫码配对。")
                }
            }
            .navigationTitle("新建密钥")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(material == nil ? "取消" : "完成") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("创建") {
                        Task { await create() }
                    }
                    .disabled(center.pendingAction != nil || trimmedName.isEmpty)
                }
            }
            .interactiveDismissDisabled(center.pendingAction != nil)
            .sheet(item: $material) { item in
                KeyMaterialSheet(material: item.material)
            }
        }
    }

    private var trimmedName: String {
        name.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func create() async {
        guard !trimmedName.isEmpty else { return }
        if let created = await center.createKey(
            name: trimmedName, allLines: allLines, manageNumber: manageNumber
        ) {
            material = PresentedKeyMaterial(material: created)
        }
    }
}

struct KeyMaterialSheet: View {
    let material: AdminWire.KeyMaterial
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Text(material.enrollmentKey)
                        .font(.system(.footnote, design: .monospaced))
                        .textSelection(.enabled)
                    Button {
                        UIPasteboard.general.string = material.enrollmentKey
                    } label: {
                        Label("复制密钥", systemImage: "doc.on.doc")
                    }
                    .accessibilityIdentifier("managementKeyCopy")
                } header: {
                    Text("配对密钥")
                } footer: {
                    Text("离开后将无法再次显示；如需再次显示，请使用「显示密钥」")
                }
                Section("配对内容") {
                    Text(material.pairingPayload)
                        .font(.system(.footnote, design: .monospaced))
                        .textSelection(.enabled)
                    Button {
                        UIPasteboard.general.string = material.pairingPayload
                    } label: {
                        Label("复制配对内容", systemImage: "doc.on.doc")
                    }
                }
            }
            .navigationTitle("密钥")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("完成") { dismiss() }
                }
            }
        }
    }
}

struct PresentedKeyMaterial: Identifiable {
    let id = UUID()
    let material: AdminWire.KeyMaterial
}
