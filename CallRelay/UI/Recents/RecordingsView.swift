import AVFoundation
import SwiftUI

/// Manual call recordings: playback, export and delete. Reachable from
/// Recents (all recordings, or one call's segment). Playback NEVER touches
/// the shared audio session while a call is live: the call path owns audio,
/// and the buttons are disabled with an explicit reason instead.
struct RecordingsView: View {
    var filterCallId: String? = nil
    var filterPeer: String? = nil

    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @StateObject private var playback = RecordingPlaybackController()
    @State private var pendingDelete: RecordingRecord?
    @State private var exportItem: RecordingExportItem?
    @State private var exportNotice: String?

    private var filtered: [RecordingRecord] {
        model.recordings
            .filter { filterCallId == nil || $0.callId == filterCallId }
            .sorted { $0.createdAt > $1.createdAt }
    }

    private var callIsActive: Bool { model.activeCall != nil }

    var body: some View {
        List {
            if callIsActive {
                Text(String(localized: "通话中暂不可播放录音"))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .accessibilityIdentifier("recordingsCallNotice")
            }
            if let notice = model.recordingNotice {
                Text(notice)
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .accessibilityIdentifier("recordingsNotice")
            }
            if let playbackError = playback.errorMessage {
                Text(playbackError)
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
            if let exportNotice {
                Text(exportNotice)
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
            if filtered.isEmpty {
                EmptyStateView(
                    icon: "waveform",
                    title: String(localized: "暂无通话录音"),
                    message: String(localized: "在通话中轻点「录音」，结束后的文件会出现在这里。")
                )
                .listRowSeparator(.hidden)
            } else {
                ForEach(filtered) { recording in
                    row(recording)
                }
            }
        }
        .navigationTitle(filterCallId == nil ? String(localized: "通话录音") : String(localized: "本次通话录音"))
        .navigationBarTitleDisplayMode(.inline)
        // An explicit, always-visible dismiss control: sheets on iPhone and
        // popover/user-driven surfaces on iPad must never trap the owner.
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button(String(localized: "完成")) { dismiss() }
                    .accessibilityIdentifier("recordingsDone")
            }
        }
        .task { await model.refreshRecordings() }
        .refreshable { await model.refreshRecordings() }
        .confirmationDialog(
            String(localized: "将从网关删除录音文件，此操作不可撤销。"),
            isPresented: Binding(
                get: { pendingDelete != nil },
                set: { if !$0 { pendingDelete = nil } }
            ),
            titleVisibility: .visible,
            presenting: pendingDelete
        ) { recording in
            Button(String(localized: "删除录音"), role: .destructive) {
                Task {
                    _ = await model.deleteRecording(recording.id)
                    pendingDelete = nil
                }
            }
            Button(String(localized: "取消"), role: .cancel) {}
        }
        .sheet(item: $exportItem) { item in
            ShareSheet(items: [item.url])
        }
        .onChange(of: model.lastDeletedRecordingId) { _, deletedId in
            if let deletedId { playback.stopIfDeleted(id: deletedId) }
        }
        // A call claiming the shared audio session must silence local
        // playback WITHOUT deactivating that session (the call owns it now).
        .onChange(of: model.activeCall?.id) { _, newValue in
            if newValue != nil { playback.stopForCallStart() }
        }
        .onDisappear { playback.stop(releaseSession: model.activeCall == nil) }
    }

    private func row(_ recording: RecordingRecord) -> some View {
        HStack(spacing: 12) {
            Button {
                Task {
                    await playback.toggle(
                        id: recording.id,
                        load: { await model.recordingData(recording.id) },
                        isCallActive: { model.activeCall != nil }
                    )
                }
            } label: {
                Image(systemName: playback.playingId == recording.id ? "stop.circle.fill" : "play.circle.fill")
                    .font(.title2)
                    .foregroundStyle(recording.isReady && !callIsActive ? Color.accentColor : Color.secondary)
                    .frame(width: 40, height: 40)
            }
            .buttonStyle(.plain)
            .disabled(!recording.isReady || callIsActive)
            .accessibilityLabel(playback.playingId == recording.id
                ? String(localized: "停止播放")
                : String(localized: "播放录音"))

            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(displayPeer(recording))
                        .font(.body)
                        .lineLimit(1)
                    if recording.isConference {
                        Text(String(localized: "会议"))
                            .font(.caption2.weight(.semibold))
                            .padding(.horizontal, 6)
                            .padding(.vertical, 2)
                            .background(Color.accentColor.opacity(0.15), in: Capsule())
                            .foregroundStyle(Color.accentColor)
                    }
                }
                Text(detail(recording))
                    .font(.caption)
                    .foregroundStyle(recording.isFailed ? Color.orange : Color.secondary)
                    .lineLimit(1)
            }

            Spacer()

            Menu {
                Button {
                    Task { await export(recording) }
                } label: {
                    Label(String(localized: "导出"), systemImage: "square.and.arrow.up")
                }
                .disabled(!recording.isReady)

                Button(role: .destructive) {
                    pendingDelete = recording
                } label: {
                    Label(String(localized: "删除"), systemImage: "trash")
                }
            } label: {
                Image(systemName: "ellipsis.circle")
                    .font(.body)
                    .foregroundStyle(.secondary)
                    .frame(width: 40, height: 40)
            }
            .accessibilityLabel(String(localized: "录音操作"))
        }
        .padding(.vertical, 2)
        .swipeActions(edge: .trailing) {
            Button(role: .destructive) {
                pendingDelete = recording
            } label: {
                Label(String(localized: "删除"), systemImage: "trash")
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("recording-\(recording.id)")
    }

    private func displayPeer(_ recording: RecordingRecord) -> String {
        if let peer = recording.peer, !peer.isEmpty {
            return model.contacts.name(forPeer: peer) ?? peer
        }
        return String(localized: "未知号码")
    }

    private func detail(_ recording: RecordingRecord) -> String {
        let formatter = DateFormatter()
        formatter.locale = Locale.current
        formatter.dateStyle = .short
        formatter.timeStyle = .short
        let date = formatter.string(from: recording.startedDate ?? recording.createdDate)
        if recording.isFailed {
            return date + " · " + String(localized: "未完成的录音")
        }
        if recording.isActive {
            return date + " · " + String(localized: "录音中…")
        }
        return date + " · " + recording.durationLabel
    }

    private func export(_ recording: RecordingRecord) async {
        exportNotice = nil
        guard let data = await model.recordingData(recording.id), !data.isEmpty else {
            exportNotice = String(localized: "无法下载录音，请稍后重试。")
            return
        }
        let name = Self.exportFileName(recording)
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(name)
        do {
            try data.write(to: url, options: .atomic)
            exportItem = RecordingExportItem(url: url)
        } catch {
            exportNotice = String(localized: "导出失败，请稍后重试。")
        }
    }

    static func exportFileName(_ recording: RecordingRecord) -> String {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        let stamp = formatter.string(from: recording.startedDate ?? recording.createdDate)
        let rawPeer = recording.peer ?? "call"
        let safePeer = rawPeer.map { $0.isLetter || $0.isNumber || $0 == "+" ? $0 : "_" }
        return "CallRelay-\(String(safePeer))-\(stamp).wav"
    }
}

/// Identifiable wrapper so the share sheet presents once a file exists.
struct RecordingExportItem: Identifiable {
    let id = UUID()
    let url: URL
}

/// Minimal playback controller for one recording at a time.
///
/// Session ownership rules:
///   * Playback is refused before/after the network fetch while a call is
///     live, so it can never steal the call's AVAudioSession.
///   * If a call starts during playback, `stopForCallStart()` stops the
///     player but deliberately does NOT call setActive(false) — the call owns
///     the session and deactivating it would cut call audio.
///   * A stale AVAudioPlayer delegate callback from a replaced player can
///     never stop the new clip or touch the session.
@MainActor
final class RecordingPlaybackController: NSObject, ObservableObject, AVAudioPlayerDelegate {
    @Published private(set) var playingId: String?
    @Published var errorMessage: String?

    private var player: AVAudioPlayer?
    private var generation = 0

    func toggle(
        id: String,
        load: @escaping () async -> Data?,
        isCallActive: @escaping () -> Bool
    ) async {
        if playingId == id {
            stop(releaseSession: true)
            return
        }
        stop(releaseSession: true)
        guard !isCallActive() else {
            errorMessage = String(localized: "通话中暂不可播放录音。")
            return
        }
        let gen = generation
        guard let data = await load() else {
            errorMessage = String(localized: "无法下载录音。")
            return
        }
        guard gen == generation, !isCallActive() else { return }
        do {
            let player = try AVAudioPlayer(data: data)
            player.delegate = self
            let session = AVAudioSession.sharedInstance()
            try session.setCategory(.playback, mode: .spokenAudio)
            try session.setActive(true)
            player.play()
            self.player = player
            playingId = id
            errorMessage = nil
        } catch {
            errorMessage = String(localized: "无法播放录音。")
        }
    }

    /// The call claimed the shared audio session: stop our player only.
    func stopForCallStart() {
        generation += 1
        player?.stop()
        player = nil
        playingId = nil
    }

    /// Normal stop. `releaseSession` must be false whenever a call is live so
    /// the shared session is never deactivated under it.
    func stop(releaseSession: Bool) {
        let hadPlayback = playingId != nil || player != nil
        stopForCallStart()
        if releaseSession && hadPlayback {
            try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        }
    }

    func stopIfDeleted(id: String) {
        if playingId == id { stop(releaseSession: true) }
    }

    nonisolated func audioPlayerDidFinishPlaying(_ player: AVAudioPlayer, successfully flag: Bool) {
        Task { @MainActor in
            guard player === self.player else { return }
            self.stop(releaseSession: true)
        }
    }

#if DEBUG
    /// Test seam: installs a playing state without touching audio hardware.
    func testingSetPlaying(id: String?, player: AVAudioPlayer?) {
        self.playingId = id
        self.player = player
    }

    var testingIsPlaying: Bool { playingId != nil || player != nil }
#endif
}
