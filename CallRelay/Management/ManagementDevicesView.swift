import SwiftUI

/// Enrolled devices with explicit revoke / allow-reenroll actions.
struct ManagementDevicesView: View {
    @ObservedObject var center: AdminCenter
    @State private var revokeCandidate: AdminWire.Device?
    @State private var reenrollCandidate: AdminWire.Device?

    private var busy: Bool { center.pendingAction != nil }

    var body: some View {
        List {
            AdminFeedbackSection(center: center)
            if center.devices.isEmpty {
                Section {
                    if center.isLoading(.devices) {
                        HStack {
                            ProgressView()
                            Text("正在读取设备…").foregroundStyle(.secondary)
                        }
                    } else {
                        Text("还没有已注册的设备。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            ForEach(center.devices) { device in
                row(device)
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("设备")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await center.refresh(.devices) }
        .task { await center.refresh(.devices) }
        .confirmationDialog(
            "撤销这台设备？",
            isPresented: Binding(
                get: { revokeCandidate != nil },
                set: { if !$0 { revokeCandidate = nil } }
            ),
            titleVisibility: .visible,
            presenting: revokeCandidate
        ) { device in
            Button("撤销这台设备", role: .destructive) {
                Task { _ = await center.revokeDevice(id: device.id) }
            }
            Button("取消", role: .cancel) {}
        } message: { _ in
            Text("该安装将不能再使用网关，也不会自动重新注册。")
        }
        .confirmationDialog(
            "允许这台设备重新注册？",
            isPresented: Binding(
                get: { reenrollCandidate != nil },
                set: { if !$0 { reenrollCandidate = nil } }
            ),
            titleVisibility: .visible,
            presenting: reenrollCandidate
        ) { device in
            Button("允许重新注册") {
                Task { _ = await center.allowDeviceReenroll(id: device.id) }
            }
            Button("取消", role: .cancel) {}
        } message: { _ in
            Text("该安装可以再次使用配对密钥注册。")
        }
    }

    @ViewBuilder
    private func row(_ device: AdminWire.Device) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(device.name).font(.body.weight(.medium))
                Spacer()
                Text(device.revoked ? "已撤销" : "正常")
                    .font(.caption)
                    .foregroundStyle(device.revoked ? AnyShapeStyle(.orange) : AnyShapeStyle(.green))
            }
            Text("密钥：\(device.keyName) · 线路数：\(device.lineCount) · 推送注册：\(device.pushRegistered ? "已注册" : "未注册")")
                .font(.caption)
                .foregroundStyle(.secondary)
            Text("注册于 \(AdminFormat.time(device.createdAt))")
                .font(.caption2)
                .foregroundStyle(.secondary)
            if device.revoked {
                Button("允许重新注册") {
                    reenrollCandidate = device
                }
                .font(.caption)
                .disabled(busy)
                .accessibilityIdentifier("managementDeviceReenroll")
            } else {
                Button("撤销这台设备", role: .destructive) {
                    revokeCandidate = device
                }
                .font(.caption)
                .disabled(busy)
                .accessibilityIdentifier("managementDeviceRevoke")
            }
        }
    }
}
