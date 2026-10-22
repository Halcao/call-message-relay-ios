import SwiftUI

/// Root of the native gateway-administrator console, reached from
/// 设置 → 网关 → 网关管理. Every action goes through `AdminCenter`, which owns
/// an administrator session independent from the device pairing.
struct ManagementView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        // Pushed from Settings' own NavigationStack: NO nested stack, so the
        // system Back button keeps working normally.
        ManagementRootView(center: model.adminCenter)
            .frame(maxWidth: 760)
            .frame(maxWidth: .infinity)
            .background(Color(.systemGroupedBackground).ignoresSafeArea())
            .navigationTitle("网关管理")
            .navigationBarTitleDisplayMode(.inline)
    }
}

private struct ManagementRootView: View {
    @ObservedObject var center: AdminCenter
    @EnvironmentObject private var model: AppModel
    @State private var didRestore = false
    @State private var showSignOutConfirm = false

    var body: some View {
        Group {
            if center.isSignedIn {
                console
            } else {
                ManagementLoginView(center: center)
            }
        }
        .task {
            guard !didRestore else { return }
            didRestore = true
            #if DEBUG
            if LaunchArguments.isManagementPreview {
                if LaunchArguments.isManagementLoginPreview {
                    // Signed-out fixture: the login form renders with the bound
                    // (synthetic) gateway origin, so an enabled form and the
                    // session-expiry recovery state are provable offline.
                    return
                }
                // DEBUG-only screenshot fixture: a synthetic signed-in console,
                // no network, no credentials.
                center.seedPreviewFixture()
                return
            }
            #endif
            await center.restore(origin: model.gatewayOrigin, gatewayId: center.gatewayId)
        }
    }

    private var console: some View {
        Form {
            // Product hierarchy (root review): the FUNCTIONS come first — the
            // operator opens this screen to do something. The status summary
            // is a compact block after them, and the account/session rows
            // (including 退出管理) sit at the BOTTOM so the actual work is
            // never buried under account chrome.
            functionsSection

            if center.errorText != nil || center.noticeText != nil {
                Section {
                    if let error = center.errorText {
                        Text(error).font(.caption).foregroundStyle(.red)
                    }
                    if let notice = center.noticeText {
                        Text(notice).font(.caption).foregroundStyle(.secondary)
                    }
                }
            }

            overviewSection

            Section {
                detailRow("网关", model.gatewayName.isEmpty ? "—" : model.gatewayName)
                detailRow("管理员", center.username.isEmpty ? "—" : center.username)
                Button("退出管理", role: .destructive) {
                    showSignOutConfirm = true
                }
                .accessibilityIdentifier("managementSignOut")
            }
        }
        .refreshable { await center.refreshAll() }
        .confirmationDialog("退出管理？", isPresented: $showSignOutConfirm, titleVisibility: .visible) {
            Button("退出管理", role: .destructive) {
                Task { await center.signOut(origin: model.gatewayOrigin) }
            }
            Button("取消", role: .cancel) {}
        } message: {
            Text("将结束这台设备上的管理会话。")
        }
    }

    /// The console's actual work, first on the screen.
    @ViewBuilder
    private var functionsSection: some View {
        Section {
            NavigationLink {
                ManagementLinesView(center: center)
            } label: {
                Label("线路", systemImage: "simcard.2")
            }
            NavigationLink {
                ManagementKeysView(center: center)
            } label: {
                Label("配对密钥", systemImage: "key.horizontal")
            }
            NavigationLink {
                ManagementDevicesView(center: center)
            } label: {
                Label("设备", systemImage: "iphone.gen3")
            }
            NavigationLink {
                ManagementCallsView(center: center)
            } label: {
                Label("通话与会议", systemImage: "phone.arrow.up.right")
            }
            NavigationLink {
                ManagementVoicemailsView(center: center)
            } label: {
                Label("语音留言", systemImage: "recordingtape")
            }
            NavigationLink {
                ManagementRecordingView(center: center)
            } label: {
                Label("录音策略", systemImage: "waveform")
            }
            NavigationLink {
                ManagementSettingsView(center: center)
            } label: {
                Label("网关设置", systemImage: "gearshape")
            }
            NavigationLink {
                ManagementDiagnosticsView(center: center)
            } label: {
                Label("诊断与恢复", systemImage: "stethoscope")
            }
        } header: {
            Text("管理")
        }
    }

    @ViewBuilder
    private var overviewSection: some View {
        Section("总览") {
            if let overview = center.overview {
                // One compact summary line instead of four full rows: the
                // functions above stay dominant.
                detailRow("状态", "\(overview.onlineLines)/\(overview.lineCount) 线路在线"
                         + " · \(overview.activeCallCount) 通话中"
                         + (overview.pushReady ? " · 推送就绪" : ""))
            } else if center.isLoading(.overview) {
                HStack {
                    ProgressView()
                    Text("正在读取…").foregroundStyle(.secondary)
                }
            } else {
                Text("尚未读取总览数据。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Button {
                Task { await center.refreshAll() }
            } label: {
                Label("刷新数据", systemImage: "arrow.clockwise")
            }
            .disabled(center.isLoading(.overview) || center.pendingAction != nil)
            .accessibilityIdentifier("managementRefresh")
        }
    }

    private func detailRow(_ title: String, _ value: String) -> some View {
        HStack {
            Text(title)
            Spacer()
            Text(value).foregroundStyle(.secondary).multilineTextAlignment(.trailing)
        }
    }
}
