import SwiftUI

/// Live calls and hosted conferences with the administrator controls: reject
/// a ringing call, hang up a live call, close a conference, remove a leg.
struct ManagementCallsView: View {
    @ObservedObject var center: AdminCenter
    @State private var hangupCandidate: AdminWire.Call?

    private var busy: Bool { center.pendingAction != nil }

    var body: some View {
        List {
            AdminFeedbackSection(center: center)
            Section("通话") {
                if center.calls.isEmpty {
                    if center.isLoading(.calls) {
                        HStack {
                            ProgressView()
                            Text("正在读取通话…").foregroundStyle(.secondary)
                        }
                    } else {
                        Text("当前没有通话记录。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                ForEach(center.calls) { call in
                    callRow(call)
                }
            }
            Section("会议") {
                if center.conferences.isEmpty {
                    Text("当前没有进行中的会议。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                ForEach(center.conferences) { conference in
                    conferenceRow(conference)
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("通话与会议")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await center.refresh(.calls) }
        .task { await center.refresh(.calls) }
        .confirmationDialog(
            "挂断这通电话？",
            isPresented: Binding(
                get: { hangupCandidate != nil },
                set: { if !$0 { hangupCandidate = nil } }
            ),
            titleVisibility: .visible,
            presenting: hangupCandidate
        ) { call in
            Button("挂断", role: .destructive) {
                Task { _ = await center.hangupCall(id: call.id) }
            }
            Button("取消", role: .cancel) {}
        } message: { call in
            Text("将结束与 \(call.peer) 的通话。")
        }
    }

    @ViewBuilder
    private func callRow(_ call: AdminWire.Call) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(call.direction == "inbound" ? "呼入" : "呼出")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text(call.peer)
                Spacer()
                Text(AdminFormat.stateLabel(call.state))
                    .font(.caption)
                    .foregroundStyle(stateColor(call.state))
            }
            Text("\(call.lineName ?? call.lineId) · \(callTiming(call))")
                .font(.caption)
                .foregroundStyle(.secondary)
            if call.held {
                Text("已保持")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            if call.state == "incoming_ringing" {
                Button("拒接", role: .destructive) {
                    Task { _ = await center.rejectCall(id: call.id) }
                }
                .font(.caption)
                .disabled(busy)
                .accessibilityIdentifier("managementCallReject")
            } else if isLive(call.state) {
                Button("挂断", role: .destructive) {
                    hangupCandidate = call
                }
                .font(.caption)
                .disabled(busy)
                .accessibilityIdentifier("managementCallHangup")
            }
        }
    }

    @ViewBuilder
    private func conferenceRow(_ conference: AdminWire.Conference) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("由 \(conference.hostName ?? "未知设备") 主持")
                Spacer()
                Text(AdminFormat.conferenceStateLabel(conference.state))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Text("成员 \(conference.legs.count) · 创建于 \(AdminFormat.time(conference.createdAt))")
                .font(.caption)
                .foregroundStyle(.secondary)
            if isLive(conference.state) {
                ForEach(conference.legs) { leg in
                    HStack {
                        Text("\(leg.peer)（\(AdminFormat.stateLabel(leg.state))）")
                            .font(.caption)
                        Spacer()
                        Button("移除") {
                            Task {
                                _ = await center.removeConferenceLeg(conferenceId: conference.id, callId: leg.id)
                            }
                        }
                        .font(.caption)
                        .disabled(busy)
                        .accessibilityIdentifier("managementConferenceLegRemove")
                    }
                }
                Button("结束会议", role: .destructive) {
                    Task { _ = await center.closeConference(id: conference.id) }
                }
                .font(.caption)
                .disabled(busy)
                .accessibilityIdentifier("managementConferenceClose")
            }
        }
    }

    private func isLive(_ state: String) -> Bool {
        switch state {
        case "", "idle", "ended", "failed":
            return false
        default:
            return true
        }
    }

    private func stateColor(_ state: String) -> AnyShapeStyle {
        switch state {
        case "incoming_ringing", "active": return AnyShapeStyle(.green)
        case "ended", "failed", "idle": return AnyShapeStyle(.secondary)
        default: return AnyShapeStyle(.orange)
        }
    }

    private func callTiming(_ call: AdminWire.Call) -> String {
        if let endedAt = call.endedAt, endedAt > 0, call.startedAt > 0 {
            return "时长 \(AdminFormat.duration(endedAt - call.startedAt))"
        }
        if isLive(call.state) {
            let from = call.connectedAt ?? call.startedAt
            if from > 0 {
                return "时长 \(AdminFormat.duration(Date().unixMilliseconds - from))"
            }
        }
        return "开始 \(AdminFormat.time(call.startedAt))"
    }
}
