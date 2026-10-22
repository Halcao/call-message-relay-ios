import SwiftUI
import AVFoundation

/// Gateway voicemail storage: totals and mount state, per-message playback and
/// swipe delete.
struct ManagementVoicemailsView: View {
    @ObservedObject var center: AdminCenter
    @EnvironmentObject private var model: AppModel
    @StateObject private var player = AdminVoicemailPlayer()
    @State private var deleteCandidate: AdminWire.Voicemail?

    private var busy: Bool { center.pendingAction != nil }

    var body: some View {
        List {
            AdminFeedbackSection(center: center)
            Section("存储") {
                HStack {
                    Text("留言占用")
                    Spacer()
                    Text(AdminFormat.bytes(center.voicemailTotalBytes)).foregroundStyle(.secondary)
                }
                if let storage = center.voicemailStorage {
                    if storage.mounted == false {
                        Text("存储不可用")
                            .font(.caption)
                            .foregroundStyle(.orange)
                        if let error = storage.error, !error.isEmpty {
                            Text(error)
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                        }
                    } else {
                        if let free = storage.freeBytes {
                            HStack {
                                Text("剩余空间")
                                Spacer()
                                Text(AdminFormat.bytes(free)).foregroundStyle(.secondary)
                            }
                        }
                        if let used = storage.usedBytes {
                            HStack {
                                Text("已用空间")
                                Spacer()
                                Text(AdminFormat.bytes(used)).foregroundStyle(.secondary)
                            }
                        }
                        if let device = storage.device, !device.isEmpty {
                            HStack {
                                Text("存储设备")
                                Spacer()
                                Text(device).foregroundStyle(.secondary)
                            }
                        }
                    }
                } else if center.isLoading(.voicemails) {
                    HStack {
                        ProgressView()
                        Text("正在读取…").foregroundStyle(.secondary)
                    }
                }
            }

            Section("留言") {
                if center.voicemails.isEmpty {
                    Text("没有语音留言。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                ForEach(center.voicemails) { voicemail in
                    row(voicemail)
                }
            }

            if let error = player.errorMessage {
                Section {
                    Text(error).font(.caption).foregroundStyle(.red)
                }
            }
        }
        .adminSectionGate(center: center)
        .navigationTitle("语音留言")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await center.refresh(.voicemails) }
        .task { await center.refresh(.voicemails) }
        .onDisappear { player.stop() }
        .confirmationDialog(
            "删除这条留言？",
            isPresented: Binding(
                get: { deleteCandidate != nil },
                set: { if !$0 { deleteCandidate = nil } }
            ),
            titleVisibility: .visible,
            presenting: deleteCandidate
        ) { voicemail in
            Button("删除", role: .destructive) {
                player.stopIfPlaying(id: voicemail.id)
                Task { _ = await center.deleteVoicemail(id: voicemail.id) }
            }
            Button("取消", role: .cancel) {}
        } message: { voicemail in
            Text("将删除来自 \(voicemail.peer) 的留言。")
        }
    }

    @ViewBuilder
    private func row(_ voicemail: AdminWire.Voicemail) -> some View {
        HStack(spacing: 12) {
            Button {
                Task { await togglePlay(voicemail) }
            } label: {
                Image(systemName: player.playingId == voicemail.id ? "stop.circle.fill" : "play.circle")
                    .font(.title2)
            }
            .buttonStyle(.plain)
            .disabled(busy)
            .accessibilityIdentifier("managementVoicemailPlay")
            VStack(alignment: .leading, spacing: 4) {
                Text(voicemail.peer)
                Text(rowSummary(voicemail))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .swipeActions(edge: .trailing) {
            Button(role: .destructive) {
                deleteCandidate = voicemail
            } label: {
                Label("删除", systemImage: "trash")
            }
            .accessibilityIdentifier("managementVoicemailDelete")
        }
    }

    private func rowSummary(_ voicemail: AdminWire.Voicemail) -> String {
        let line = voicemail.lineName ?? voicemail.lineId
        return "\(line) · \(AdminFormat.duration(voicemail.durationMs)) · \(AdminFormat.bytes(voicemail.sizeBytes)) · 到期 \(AdminFormat.time(voicemail.expiresAt))"
    }

    private func togglePlay(_ voicemail: AdminWire.Voicemail) async {
        if player.playingId == voicemail.id {
            player.stop()
            return
        }
        guard model.activeCall == nil else {
            player.errorMessage = "通话中暂不可播放留言。"
            return
        }
        guard let data = await center.voicemailAudio(id: voicemail.id) else { return }
        player.play(data, id: voicemail.id)
    }
}

/// One-at-a-time voicemail playback. The downloaded bytes are written to a
/// temp file, played with `AVAudioPlayer`, and the file is removed on stop or
/// natural completion. Never touches the shared audio session while a call is
/// live (the view checks before downloading).
@MainActor
final class AdminVoicemailPlayer: NSObject, ObservableObject, AVAudioPlayerDelegate {
    @Published private(set) var playingId: String?
    @Published var errorMessage: String?

    private var player: AVAudioPlayer?
    private var fileURL: URL?

    func play(_ data: Data, id: String) {
        stop()
        let safeName = id.addingPercentEncoding(withAllowedCharacters: .alphanumerics) ?? UUID().uuidString
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("admin-voicemail-\(safeName).caf")
        do {
            try data.write(to: url, options: .atomic)
            let player = try AVAudioPlayer(contentsOf: url)
            player.delegate = self
            player.prepareToPlay()
            let session = AVAudioSession.sharedInstance()
            try session.setCategory(.playback, mode: .spokenAudio)
            try session.setActive(true)
            guard player.play() else {
                throw CocoaError(.fileReadUnknown)
            }
            self.player = player
            fileURL = url
            playingId = id
            errorMessage = nil
        } catch {
            try? FileManager.default.removeItem(at: url)
            errorMessage = "无法播放留言。"
        }
    }

    func stop() {
        let hadPlayback = player != nil || playingId != nil
        player?.delegate = nil
        player?.stop()
        player = nil
        playingId = nil
        if let fileURL {
            try? FileManager.default.removeItem(at: fileURL)
        }
        fileURL = nil
        if hadPlayback {
            try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        }
    }

    func stopIfPlaying(id: String) {
        if playingId == id { stop() }
    }

    nonisolated func audioPlayerDidFinishPlaying(_ player: AVAudioPlayer, successfully flag: Bool) {
        Task { @MainActor in
            guard player === self.player else { return }
            self.stop()
        }
    }
}
