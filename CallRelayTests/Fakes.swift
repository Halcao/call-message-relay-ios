import Foundation
import CallKit
@testable import CallRelay

// MARK: - In-memory keychain

final class DictionaryKeychain: KeychainWrapping {
    var storage: [String: Data] = [:]
    private func key(_ service: String, _ account: String) -> String { "\(service)|\(account)" }

    func readData(service: String, account: String) -> Data? { storage[key(service, account)] }

    func saveData(_ data: Data, service: String, account: String, accessibility: KeychainAccessibility) throws {
        storage[key(service, account)] = data
    }

    func delete(service: String, account: String) { storage.removeValue(forKey: key(service, account)) }
}

// MARK: - Fake media

final class FakeMediaSession: CallMediaSession {
    var onState: ((MediaState) -> Void)?
    var onQuality: ((MediaQuality) -> Void)?
    var makeOfferError: Error?
    var closeCount = 0
    var activationCount = 0
    var muted = false
    var speaker = false
    var afterApplyAnswer: (() -> Void)?
    var makeOfferCount = 0
    /// Direct in-app answer path activations (no CallKit didActivate).
    var activateWithoutCallKitCount = 0
    var deactivateWithoutCallKitCount = 0
    /// When false, a direct-answer session activation fails.
    var activateWithoutCallKitResult = true

    func makeOffer(ice: ICEConfiguration, relayOnly: Bool) async throws -> String {
        makeOfferCount += 1
        if let makeOfferError { throw makeOfferError }
        return "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\na=rtpmap:0 PCMU/8000\r\n"
    }

    func applyAnswer(_ sdp: String) async throws {
        afterApplyAnswer?()
    }

    func setMicMuted(_ muted: Bool) { self.muted = muted }
    func setSpeakerphone(_ enabled: Bool) throws { speaker = enabled }
    func audioActivated(with session: AVAudioSession) { activationCount += 1 }
    func audioDeactivated(with session: AVAudioSession) {}
    func activateAudioWithoutCallKit() -> Bool {
        activateWithoutCallKitCount += 1
        return activateWithoutCallKitResult
    }
    func deactivateAudioWithoutCallKit() { deactivateWithoutCallKitCount += 1 }
    private var freshMediaCallback: (() -> Void)?
    func setRecoveryFreshMedia(_ callback: (() -> Void)?) { freshMediaCallback = callback }
    #if DEBUG
    func debugFireFreshMedia() { freshMediaCallback?() }
    #endif
    func close() { closeCount += 1 }
}

import AVFoundation

@MainActor
final class FakeMediaProvider: MediaSessionProviding {
    let session: FakeMediaSession
    var createCount = 0
    /// When true, each request yields a fresh fake so per-call teardown can be
    /// observed (the default shared instance mirrors the old single-call tests).
    var createsNewSessions = false
    private(set) var created: [FakeMediaSession] = []

    init(session: FakeMediaSession) { self.session = session }

    func makeSession() -> CallMediaSession {
        createCount += 1
        if createsNewSessions {
            let clone = FakeMediaSession()
            created.append(clone)
            return clone
        }
        return session
    }
}

// MARK: - Fake CallKit

@MainActor
final class FakeCallKit: CallKitControlling {
    weak var director: CallDirecting?
    var incoming: [(uuid: UUID, handle: String)] = []
    var updates: [(uuid: UUID, handle: String)] = []
    var connecting: [UUID] = []
    var connected: [UUID] = []
    var ended: [(uuid: UUID, reason: CXCallEndedReason)] = []
    var startRequests: [UUID] = []
    var heldReports: [(uuid: UUID, held: Bool)] = []
    /// When false, `reportIncoming` mimics a CallKit rejection.
    var reportIncomingResult = true
    /// Fail the first `reportIncoming` after arming; continuations resume
    /// FIFO with `resumeReport`. Lets tests interleave ends/resets/overlapping
    /// pushes with the awaits.
    var armReportWait = false
    private var reportWaiters: [CheckedContinuation<Bool, Never>] = []
    func resumeReport(_ accepted: Bool) {
        guard !reportWaiters.isEmpty else { return }
        reportWaiters.removeFirst().resume(returning: accepted)
    }
    /// Raw `CXErrorCodeIncomingCallError` reported with a rejection.
    var reportIncomingErrorCode: Int?
    private(set) var lastIncomingReportErrorCode: Int?
    /// When set, `requestAnswer` fails like an unknown/ended system call.
    var requestAnswerError: Error?
    var requestAnswerCalls: [UUID] = []
    /// Simulates the provider delegate reacting to a successful answer.
    var onRequestAnswer: ((UUID) -> Void)?
    var requestEndError: Error?
    var requestEndCalls: [UUID] = []

    func reportIncoming(uuid: UUID, handle: String, isVideo: Bool) async -> Bool {
        incoming.append((uuid, handle))
        if armReportWait {
            armReportWait = false
            let accepted = await withCheckedContinuation { reportWaiters.append($0) }
            lastIncomingReportErrorCode = accepted ? nil : (reportIncomingErrorCode ?? 0)
            return accepted
        }
        lastIncomingReportErrorCode = reportIncomingResult ? nil : (reportIncomingErrorCode ?? 0)
        return reportIncomingResult
    }
    func updateIncoming(uuid: UUID, handle: String) { updates.append((uuid, handle)) }
    func requestStartOutgoing(uuid: UUID, handle: String) async throws { startRequests.append(uuid) }
    func reportOutgoingConnecting(uuid: UUID) { connecting.append(uuid) }
    func reportConnected(uuid: UUID, startedAt: Date?) { connected.append(uuid) }
    func reportEnded(uuid: UUID, reason: CXCallEndedReason) async { ended.append((uuid, reason)) }
    func reportHeld(uuid: UUID, held: Bool) { heldReports.append((uuid, held)) }
    func requestEnd(uuid: UUID) async throws {
        requestEndCalls.append(uuid)
        if let requestEndError { throw requestEndError }
    }
    func requestAnswer(uuid: UUID) async throws {
        requestAnswerCalls.append(uuid)
        if let requestAnswerError { throw requestAnswerError }
        onRequestAnswer?(uuid)
    }
    func requestMute(uuid: UUID, muted: Bool) async throws {}
    func requestDTMF(uuid: UUID, digit: String) async throws {}
    func invalidate() {}
}

// MARK: - Fake gateway with controllable timing

@MainActor
class FakeGatewayAPI: GatewayAPI {
    struct IdemCall { let id: String; let key: String; let to: String }

    var identityResponse: IdentityResponse?
    var dialResult: Result<CallRecord, Error> = .failure(APIError.notReady(""))
    var offerError: Error?
    var iceError: Error?
    var activeRecordForPoll: CallRecord?

    var dials: [IdemCall] = []
    var answers: [String] = []
    var rejects: [String] = []
    var hangups: [String] = []
    var dtmfs: [(id: String, digit: String)] = []
    var offers: [String] = []

    // Unified multi-call / conference surface
    var holdError: Error?
    var resumeError: Error?
    var holds: [String] = []
    var resumes: [String] = []
    var dialLines: [String?] = []
    var setNumberCalls: [(lineId: String, number: String)] = []
    var setNumberResult: Result<AuthorizedLine, Error> = .failure(APIError.notReady("set number not configured"))
    var authorizedLinesStub: [AuthorizedLine] = []
    var authorizedLinesError: Error?
    private(set) var authorizedLinesCallCount = 0
    /// When armed, the next authorizedLines() call blocks until
    /// resumeAuthorizedLines; used to deliver a stale response after rebind.
    private var linesWaiter: CheckedContinuation<[AuthorizedLine], Error>?
    private var linesArmed = false
    private var numberWaiter: CheckedContinuation<AuthorizedLine, Error>?
    private var numberArmed = false
    var mergeError: Error?

    // MARK: Direct route (probe/commit/rollback)
    var attachProbeResult: Result<WebRTCAnswer, Error> = .success(
        WebRTCAnswer(sdp: "v=0\r\n", type: "answer", iceMode: "all"))
    /// Per-call queued attach results, consumed in order (falling back to
    /// `attachProbeResult`); lets tests script fail-then-succeed retries.
    var attachProbeResultQueue: [Result<WebRTCAnswer, Error>] = []
    private(set) var attachProbeCalls: [String] = []
    /// When armed once, the next attach parks until resumeAttach/cancel
    /// (hung-attach routing tests). Cancellation mirrors a real
    /// URLSession.data(for:) throwing on task cancel.
    private var attachArmed = false
    private var attachContinuation: CheckedContinuation<WebRTCAnswer, Error>?
    private(set) var attachCancelled = false

    func armAttachWait() { attachArmed = true }
    func resumeAttach(with result: Result<WebRTCAnswer, Error>) {
        attachContinuation?.resume(with: result)
        attachContinuation = nil
    }

    var commitError: Error?
    private(set) var commitCalls: [String] = []
    /// Build-50: commits split by role — PRIMARY (legacy replace) vs STANDBY.
    /// Used for negative assertions that legacy probing never runs.
    private(set) var primaryCommitCalls: [String] = []
    private(set) var standbyCommitCalls: [String] = []
    private(set) var preflightCommitIds: [String?] = []
    private(set) var discardPreflightIds: [String] = []
    private(set) var deletedThreadKeys: [String] = []
    /// Per-key delete errors so bulk partial-failure behavior is testable.
    var deleteThreadErrors: [String: Error] = [:]
    /// Per-message read errors so bulk mark-read partial failures are testable.
    var readErrors: [String: Error] = [:]
    private var commitContinuation: CheckedContinuation<Void, Error>?
    private var commitArmed = false
    var discardProbeCalls: [String] = []
    var measureRequestCallCount = 0
    var relayProbeRequestCallCount = 0

    func armCommitWait() { commitArmed = true }
    var onCommit: (() -> Void)?
    func resumeCommit(with result: Result<Void, Error>) {
        commitContinuation?.resume(with: result)
        commitContinuation = nil
    }

    func armAuthorizedLinesWait() { linesArmed = true }
    func resumeAuthorizedLines(with result: Result<[AuthorizedLine], Error>) {
        if let waiter = linesWaiter {
            linesWaiter = nil
            waiter.resume(with: result)
        }
    }
    func armSetNumberWait() { numberArmed = true }
    func resumeSetNumber(with result: Result<AuthorizedLine, Error>) {
        if let waiter = numberWaiter {
            numberWaiter = nil
            waiter.resume(with: result)
        }
    }
    var mergeResult: ConferenceRecord?
    var merges: [[String]] = []
    var conferenceOffers: [(conferenceId: String, sdp: String)] = []
    var closeConferences: [String] = []
    var removedLegs: [(conferenceId: String, callId: String)] = []
    var legHolds: [(conferenceId: String, callId: String, held: Bool)] = []
    var legDTMFs: [(conferenceId: String, callId: String, digit: String)] = []
    var splits: [(conferenceId: String, callId: String)] = []
    var conferenceSnapshot: ConferenceRecord?
    /// Ordered trace of call-affecting commands, used to assert ordering.
    var actionLog: [String] = []

    var smsActionsSupported = false
    var smsActionBatches: [[SMSConversationAction]] = []
    var smsActionAsyncResult: (([SMSConversationAction]) async throws -> SMSActionResponse)?
    var smsActionResult: (([SMSConversationAction]) throws -> SMSActionResponse)?
    func supportsSMSActions() async throws -> Bool { smsActionsSupported }
    func performSMSActions(_ actions: [SMSConversationAction]) async throws -> SMSActionResponse {
        smsActionBatches.append(actions)
        if let smsActionAsyncResult { return try await smsActionAsyncResult(actions) }
        if let smsActionResult { return try smsActionResult(actions) }
        return SMSActionResponse(results: actions.map { .init(id: $0.id, ok: true, error: nil) })
    }

    // SMS surface
    var threads: [MessageThread] = []
    var threadPages: [String: [MessageRecord]] = [:]
    /// Per-key errors so tests can make one line candidate fail while
    /// another succeeds (unqualified-key resolution order).
    var threadPageErrors: [String: Error] = [:]
    /// Every threadKey requested via listThreadMessages, in order.
    private(set) var requestedThreadKeys: [String] = []
    var threadHasMore = false
    var sentMessages: [SentSMS] = []
    /// Parallel to `sentMessages`: the line each send requested.
    var sentLineIDs: [String?] = []
    var sendResult: Result<MessageRecord, Error> = .failure(APIError.notReady("send not configured"))
    var idempotentReplays: [String: MessageRecord] = [:]
    var readMarked: [String] = []
    var onSendEntered: ((String) -> Void)?
    private var sendContinuation: CheckedContinuation<MessageRecord, Error>?
    var autoResumeSend = true

    // Voicemail surface
    var voicemailsStub: [VoicemailRecord] = []
    var voicemailDeleteResult: Result<Void, Error> = .success(())
    private(set) var voicemailDeletes: [String] = []

    // Manual call recording surface
    var recordingsStub: [RecordingRecord] = []
    var recordingStartResult: Result<RecordingRecord, Error> = .failure(APIError.notReady("recording not configured"))
    var recordingStopResult: Result<RecordingRecord, Error> = .failure(APIError.notReady("recording not configured"))
    var recordingAudioStub: Data?
    var recordingDeleteResult: Result<Void, Error> = .success(())
    private(set) var recordingStarts: [String] = []
    private(set) var recordingStops: [String] = []
    private(set) var recordingDeletes: [String] = []

    struct SentSMS { let to: String; let body: String; let key: String }

    var onDialEntered: (() -> Void)?
    private var dialContinuation: CheckedContinuation<CallRecord, Error>?
    var autoResumeDial = true

    func identity() async throws -> IdentityResponse {
        if let identityResponse { return identityResponse }
        throw APIError.network(URLError(.cannotFindHost))
    }
    func gatewayInfo() async throws -> GatewayResponse {
        GatewayResponse(id: "gw", name: "GW", lineID: nil, transport: "tailnet", capabilities: nil)
    }
    var lineError: Error?
    private(set) var lineCallCount = 0
    private var lineWaiter: CheckedContinuation<LineStatus, Error>?
    private var lineArmed = false
    func armLineWait() { lineArmed = true }
    func resumeLine(with result: Result<LineStatus, Error>) {
        if let waiter = lineWaiter {
            lineWaiter = nil
            waiter.resume(with: result)
        }
    }
    func line() async throws -> LineStatus {
        lineCallCount += 1
        if lineArmed {
            lineArmed = false
            return try await withCheckedThrowingContinuation { cont in
                lineWaiter = cont
            }
        }
        if let lineError { throw lineError }
        return LineStatus.demoReady()
    }
    func listCalls(limit: Int) async throws -> [CallRecord] { listCallsStub }
    /// Backing store for listCalls/listCallsConditional (defaults to empty,
    /// matching the previous stub behavior).
    var listCallsStub: [CallRecord] { callsStub }
    func listVoicemails() async throws -> [VoicemailRecord] { voicemailsStub }
    func deleteVoicemail(id: String) async throws {
        voicemailDeletes.append(id)
        try voicemailDeleteResult.get()
    }
    func startCallRecording(callId: String) async throws -> RecordingRecord {
        recordingStarts.append(callId)
        return try recordingStartResult.get()
    }
    func stopCallRecording(callId: String) async throws -> RecordingRecord {
        recordingStops.append(callId)
        return try recordingStopResult.get()
    }
    var conferenceRecordingStartResult: Result<RecordingRecord, Error> =
        .failure(APIError.notReady("conference recording not configured"))
    var conferenceRecordingStopResult: Result<RecordingRecord, Error> =
        .failure(APIError.notReady("conference recording not configured"))
    private(set) var conferenceRecordingStarts: [String] = []
    private(set) var conferenceRecordingStops: [String] = []
    func startConferenceRecording(conferenceId: String) async throws -> RecordingRecord {
        conferenceRecordingStarts.append(conferenceId)
        return try conferenceRecordingStartResult.get()
    }
    func stopConferenceRecording(conferenceId: String) async throws -> RecordingRecord {
        conferenceRecordingStops.append(conferenceId)
        return try conferenceRecordingStopResult.get()
    }
    func listRecordings() async throws -> [RecordingRecord] { recordingsStub }
    func recordingAudio(id: String) async throws -> Data {
        guard let recordingAudioStub else { throw APIError.notReady("no audio") }
        return recordingAudioStub
    }
    func deleteRecording(id: String) async throws {
        recordingDeletes.append(id)
        try recordingDeleteResult.get()
    }
    /// Reconciliation stub: tests arm the gateway's active set explicitly.
    var activeCallsStub: [CallRecord] = []
    var activeCallsError: Error?
    private(set) var activeCallsCallCount = 0
    func activeCalls() async throws -> [CallRecord] {
        activeCallsCallCount += 1
        if let activeCallsError { throw activeCallsError }
        return activeCallsStub
    }
    /// When armed, the next fetchCall parks until resumeFetchCall, so tests
    /// can interleave a terminal event during the verification await.
    private var fetchWaiter: CheckedContinuation<CallRecord, Error>?
    private var fetchArmed = false
    func armFetchCallWait() { fetchArmed = true }
    var fetchCallParked: Bool { fetchWaiter != nil }
    func resumeFetchCall(with result: Result<CallRecord, Error>) {
        fetchWaiter?.resume(with: result)
        fetchWaiter = nil
    }
    func fetchCall(id: String) async throws -> CallRecord {
        if fetchArmed {
            fetchArmed = false
            return try await withCheckedThrowingContinuation { cont in
                fetchWaiter = cont
            }
        }
        if let match = activeCallsStub.first(where: { $0.id == id }) { return match }
        if let activeRecordForPoll { return activeRecordForPoll }
        throw APIError.http(status: 404, code: "CB-CALL-006", message: "not found")
    }

    func dial(to: String, clientCallId: String, idempotencyKey: String) async throws -> CallRecord {
        dials.append(IdemCall(id: clientCallId, key: idempotencyKey, to: to))
        actionLog.append("dial:\(to)")
        onDialEntered?()
        if autoResumeDial {
            return try dialResult.get()
        }
        return try await withCheckedThrowingContinuation { cont in
            dialContinuation = cont
        }
    }

    func dial(to: String, lineId: String?, clientCallId: String, idempotencyKey: String) async throws -> CallRecord {
        dialLines.append(lineId)
        if let lineId { actionLog.append("dialLine:\(lineId)") }
        return try await dial(to: to, clientCallId: clientCallId, idempotencyKey: idempotencyKey)
    }

    func resumeDial(_ result: Result<CallRecord, Error>) {
        dialContinuation?.resume(with: result)
        dialContinuation = nil
    }

    // MARK: Unified lines

    func authorizedLines() async throws -> [AuthorizedLine] {
        authorizedLinesCallCount += 1
        if let authorizedLinesError { throw authorizedLinesError }
        if linesArmed {
            linesArmed = false
            return try await withCheckedThrowingContinuation { cont in
                linesWaiter = cont
            }
        }
        return authorizedLinesStub
    }

    func setLineNumber(_ lineId: String, phoneNumber: String) async throws -> AuthorizedLine {
        setNumberCalls.append((lineId, phoneNumber))
        if numberArmed {
            numberArmed = false
            return try await withCheckedThrowingContinuation { cont in
                numberWaiter = cont
            }
        }
        return try setNumberResult.get()
    }

    var setDefaultLineCalls: [String] = []
    var setDefaultLineError: Error?
    /// Per-line artificial latency so tests can force an older, slower
    /// preference PUT to overlap a newer choice.
    var setDefaultLineDelays: [String: TimeInterval] = [:]
    func setDefaultLine(_ lineId: String, idempotencyKey: String) async throws {
        if let delay = setDefaultLineDelays[lineId], delay > 0 {
            try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
        }
        if let setDefaultLineError { throw setDefaultLineError }
        setDefaultLineCalls.append(lineId)
    }

    var denoiseCalls: [(uplink: Bool?, downlink: Bool?)] = []
    func setDenoisePreferences(uplink: Bool?, downlink: Bool?) async throws {
        denoiseCalls.append((uplink, downlink))
    }

    func answer(callId: String, idempotencyKey: String) async throws {
        answers.append(callId)
        actionLog.append("answer:\(callId)")
    }
    func reject(callId: String, idempotencyKey: String) async throws { rejects.append(callId) }
    func hangup(callId: String, idempotencyKey: String) async throws {
        hangups.append(callId)
        actionLog.append("hangup:\(callId)")
    }
    func hold(callId: String, idempotencyKey: String) async throws {
        holds.append(callId)
        actionLog.append("hold:\(callId)")
        if let holdError { throw holdError }
    }
    func resume(callId: String, idempotencyKey: String) async throws {
        resumes.append(callId)
        actionLog.append("resume:\(callId)")
        if let resumeError { throw resumeError }
    }
    func dtmf(callId: String, digit: String, idempotencyKey: String) async throws {
        dtmfs.append((callId, digit))
    }

    func webRTCOffer(callId: String, sdp: String, transport: String, idempotencyKey: String) async throws -> WebRTCAnswer {
        offers.append(callId)
        if let offerError { throw offerError }
        return WebRTCAnswer(sdp: "v=0\r\n", type: "answer", iceMode: "relay")
    }

    /// When set, returned instead of the default ICE-only configuration
    /// (e.g. to advertise the WSS media transport).
    var iceConfigOverride: ICEConfiguration?
    var standbyWebSocketCalls: [String] = []
    /// When set, returned by mediaWebSocketRequest (default throws notReady).
    var mediaWSRequestOverride: URLRequest?
    /// Build-49: when set, returned by conferenceMediaWebSocketRequest.
    var conferenceMediaWSRequestOverride: URLRequest?

    func conferenceMediaWebSocketRequest(conferenceId: String, denoise: DenoiseChoice?) async throws -> URLRequest {
        if let conferenceMediaWSRequestOverride { return conferenceMediaWSRequestOverride }
        throw APIError.notReady("当前配对不支持会议 WebSocket 音频。")
    }
    /// Count of call media WebSocket attach requests (build-38 fastpath tests).
    private(set) var mediaWSRequestCallCount = 0

    func iceConfiguration(callId: String) async throws -> ICEConfiguration {
        if let iceError { throw iceError }
        if let iceConfigOverride { return iceConfigOverride }
        return ICEConfiguration(
            policy: "tailnet-turn",
            iceServers: [ICEServer(urls: ["turn:turn.example:3478?transport=udp"], username: "u", credential: "c")],
            expiresAt: "2026-10-01T00:00:00Z"
        )
    }

    var standbyWebSocketResult: URLRequest?
    func mediaWebSocketStandbyRequest(callId: String) async throws -> URLRequest {
        standbyWebSocketCalls.append(callId)
        if let standbyWebSocketResult { return standbyWebSocketResult }
        throw APIError.notReady("standby unsupported")
    }
    func mediaWebSocketRequest(callId: String, denoise: DenoiseChoice? = nil) async throws -> URLRequest {
        mediaWSRequestCallCount += 1
        if let mediaWSRequestOverride { return mediaWSRequestOverride }
        throw APIError.notReady("当前配对不支持 WebSocket 音频。")
    }

    func sync(after: Int64, limit: Int) async throws -> SyncResponse {
        SyncResponse(from: after, to: after, hasMore: false, changes: [])
    }
    func registerPush(registration: PushRegistration, idempotencyKey: String) async throws {}

    // MARK: Contacts sync + service alerts (unified v2)

    /// Uploaded contact batches in order.
    private(set) var contactSyncRequests: [ContactSyncRequest] = []
    /// When set, syncContacts throws it.
    var contactSyncError: Error?
    /// Result returned by syncContacts (defaults to a successful merge).
    var contactSyncResult = ContactSyncResult(revision: 1, created: 0, updated: 0, unchanged: 0, skipped: 0, total: 0)
    /// When set, notificationsStatus throws it.
    var notificationsStatusError: Error?
    var notificationsStatusResult: GatewayNotificationStatus?

    func syncContacts(_ request: ContactSyncRequest, idempotencyKey: String) async throws -> ContactSyncResult {
        contactSyncRequests.append(request)
        actionLog.append("syncContacts:\(request.contacts.count)")
        if let contactSyncError { throw contactSyncError }
        return ContactSyncResult(
            revision: contactSyncResult.revision,
            created: contactSyncResult.created + request.contacts.count,
            updated: contactSyncResult.updated,
            unchanged: contactSyncResult.unchanged,
            skipped: contactSyncResult.skipped,
            total: request.contacts.count
        )
    }

    func notificationsStatus() async throws -> GatewayNotificationStatus {
        if let notificationsStatusError { throw notificationsStatusError }
        if let notificationsStatusResult { return notificationsStatusResult }
        throw APIError.notReady("提醒状态未配置")
    }

    // MARK: Conference (unified v2)

    func merge(calls: [String], idempotencyKey: String) async throws -> ConferenceRecord {
        merges.append(calls)
        actionLog.append("merge:\(calls.joined(separator: ","))")
        if let mergeError { throw mergeError }
        if let mergeResult { return mergeResult }
        throw APIError.notReady("会议未配置")
    }

    func conference(id: String) async throws -> ConferenceRecord {
        if let conferenceSnapshot { return conferenceSnapshot }
        throw APIError.notReady("会议未配置")
    }

    func conferenceOffer(
        conferenceId: String, sdp: String, idempotencyKey: String
    ) async throws -> WebRTCAnswer {
        conferenceOffers.append((conferenceId, sdp))
        actionLog.append("conferenceOffer:\(conferenceId)")
        if let offerError { throw offerError }
        return WebRTCAnswer(sdp: "v=0\r\n", type: "answer", iceMode: "relay")
    }

    func closeConference(id: String, idempotencyKey: String) async throws {
        closeConferences.append(id)
        actionLog.append("closeConference:\(id)")
        if closeArmed {
            closeArmed = false
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
                closeContinuation = cont
            }
            return
        }
        if let closeError { throw closeError }
    }
    /// Build-49: when armed once, the next closeConference parks until
    /// resumeCloseConference (hung-close expiry test).
    private var closeArmed = false
    private var closeContinuation: CheckedContinuation<Void, Error>?
    var closeError: Error?
    func armCloseConferenceWait() { closeArmed = true }
    var closeConferenceParked: Bool { closeContinuation != nil }
    func resumeCloseConference(with result: Result<Void, Error>) {
        closeContinuation?.resume(with: result)
        closeContinuation = nil
    }

    func removeConferenceLeg(conferenceId: String, callId: String, idempotencyKey: String) async throws {
        removedLegs.append((conferenceId, callId))
        actionLog.append("removeLeg:\(callId)")
    }

    func setConferenceLegHeld(conferenceId: String, callId: String, held: Bool, idempotencyKey: String) async throws {
        legHolds.append((conferenceId, callId, held))
        actionLog.append(held ? "legHold:\(callId)" : "legResume:\(callId)")
    }

    func conferenceLegDTMF(conferenceId: String, callId: String, digit: String, idempotencyKey: String) async throws {
        legDTMFs.append((conferenceId, callId, digit))
    }

    func splitConference(id: String, callId: String, idempotencyKey: String) async throws {
        splits.append((id, callId))
        actionLog.append("split:\(callId)")
    }

    // MARK: SMS

    func listThreads() async throws -> [MessageThread] {
        if listThreadsSuspended {
            listThreadsSuspended = false
            listThreadsConfirmed = true
            await withCheckedContinuation { listThreadsContinuation = $0 }
        }
        return threads
    }

    /// Per-line content for line-filter variant tests (nil = every request
    /// serves `threads`, the historical behavior).
    var threadsByLine: [String?: [MessageThread]]?
    func listThreads(lineId: String?) async throws -> [MessageThread] {
        if let map = threadsByLine {
            return map[lineId] ?? []
        }
        return try await listThreads()
    }

    /// Build-67 revision-fence tests: park the NEXT list GET so a mark ACK
    /// can land while it is still in flight.
    var listThreadsConfirmed = false
    private var listThreadsSuspended = false
    private var listThreadsContinuation: CheckedContinuation<Void, Never>?
    func suspendNextListThreads() {
        listThreadsSuspended = true
        listThreadsConfirmed = false
    }
    func resumeListThreads() {
        listThreadsContinuation?.resume()
        listThreadsContinuation = nil
    }

    func listMessages(after: Int64, limit: Int) async throws -> [MessageRecord] {
        listMessagesAfterCursors.append(after)
        return extraMessages
    }
    var extraMessages: [MessageRecord] = []

    func listThreadMessages(
        threadKey: String, beforeCreatedAt: Int64?, beforeID: String?, limit: Int
    ) async throws -> ThreadMessagePage {
        requestedThreadKeys.append(threadKey)
        if let error = threadPageErrors[threadKey] { throw error }
        return ThreadMessagePage(messages: threadPages[threadKey] ?? [], hasMore: threadHasMore)
    }

    func sendMessage(to: String, body: String, idempotencyKey: String) async throws -> MessageRecord {
        try await sendMessage(to: to, body: body, lineId: nil, idempotencyKey: idempotencyKey)
    }

    /// Captures the requested line so tests can prove per-line send/retry
    /// behavior (a retry must reuse the entry's captured line).
    func sendMessage(to: String, body: String, lineId: String?, idempotencyKey: String) async throws -> MessageRecord {
        if let replay = idempotentReplays[idempotencyKey] { return replay }
        sentMessages.append(SentSMS(to: to, body: body, key: idempotencyKey))
        sentLineIDs.append(lineId)
        onSendEntered?(idempotencyKey)
        if autoResumeSend {
            return try sendResult.get()
        }
        return try await withCheckedThrowingContinuation { cont in
            sendContinuation = cont
        }
    }

    func resumeSend(_ result: Result<MessageRecord, Error>) {
        sendContinuation?.resume(with: result)
        sendContinuation = nil
    }

    func markMessageRead(id: String, idempotencyKey: String) async throws {
        if let error = readErrors[id] { throw error }
        if holdReads {
            try await withCheckedThrowingContinuation { readContinuation = $0 }
            holdReads = false
        }
        readMarked.append(id)
    }

    /// Build-67 responsiveness tests: park each read-mark until resumed.
    var holdReads = false
    private var readContinuation: CheckedContinuation<Void, Error>?
    func armReadHold() { holdReads = true }
    func resumeRead(_ result: Result<Void, Error> = .success(())) {
        holdReads = false
        readContinuation?.resume(with: result)
        readContinuation = nil
    }


    func attachMediaProbe(callId: String, sdp: String) async throws -> WebRTCAnswer {
        attachProbeCalls.append(callId)
        if attachArmed {
            attachArmed = false
            return try await withTaskCancellationHandler {
                try await withCheckedThrowingContinuation { (cont: CheckedContinuation<WebRTCAnswer, Error>) in
                    self.attachContinuation = cont
                }
            } onCancel: { [weak self] in
                Task { @MainActor in
                    guard let self else { return }
                    self.attachCancelled = true
                    self.attachContinuation?.resume(throwing: CancellationError())
                    self.attachContinuation = nil
                }
            }
        }
        if !attachProbeResultQueue.isEmpty {
            return try attachProbeResultQueue.removeFirst().get()
        }
        return try attachProbeResult.get()
    }
    func commitMediaProbe(callId: String, preflightId: String?) async throws {
        commitCalls.append(callId)
        primaryCommitCalls.append(callId)
        preflightCommitIds.append(preflightId)
        onCommit?()
        if commitArmed {
            commitArmed = false
            try await withCheckedThrowingContinuation { commitContinuation = $0 }
            return
        }
        if let commitError { throw commitError }
    }
    // Build-50 hot-standby fakes: recorded for controller tests.
    var standbyCommitResult: V2StandbyFence = V2StandbyFence(epoch: 1, token: "fake-token")
    var standbyCommitError: Error?
    var standbyPromoteResult: V2PromoteStandbyResponse = V2PromoteStandbyResponse(epoch: 2, currentEpoch: 2, primary: "ws", standby: "ice", standbyToken: "demoted-token")
    var standbyPromoteError: Error?
    var standbyStateResult: V2MediaStateResponse = V2MediaStateResponse(primary: "ws", epoch: 1, standbyWs: false, standbyIce: true, standbyWsToken: nil, standbyIceToken: "ice-tok")
    var standbyStateError: Error?
    func commitMediaProbe(callId: String, preflightId: String?, asStandby: Bool) async throws -> V2StandbyFence {
        commitCalls.append(callId)
        standbyCommitCalls.append(callId)
        preflightCommitIds.append(preflightId)
        onCommit?()
        if let standbyCommitError { throw standbyCommitError }
        return standbyCommitResult
    }
    var standbyPromoteCalls: [(transport: String, token: String, expectEpoch: UInt64)] = []
    /// Build-51 rollback validation: per-call scripted promote outcomes
    /// (one entry consumed per call). Models a swap-back that is rejected
    /// or acked with DIFFERENT roles than the canned default, so tests
    /// prove the role rather than merely count the request.
    var standbyPromoteScript: [Result<V2PromoteStandbyResponse, Error>] = []
    /// When true the promote parks until resumeStandbyPromote(): an
    /// in-flight HTTP ack a stale lifecycle can deliver late.
    var standbyPromoteArmed = false
    /// Build-51: parks the Nth promoteStandby call (1-based) until
    /// resumeStandbyPromote() — models an in-flight swap-back while the
    /// call ends, without arming the promotion command itself.
    var standbyPromoteParkOnCall: Int?
    private var standbyPromoteContinuation: CheckedContinuation<V2PromoteStandbyResponse, Error>?
    /// Resumes a parked promote with the CURRENT result/error config.
    func resumeStandbyPromote() {
        standbyPromoteArmed = false
        guard let cont = standbyPromoteContinuation else { return }
        standbyPromoteContinuation = nil
        if let standbyPromoteError {
            cont.resume(throwing: standbyPromoteError)
        } else {
            cont.resume(returning: standbyPromoteResult)
        }
    }
    @discardableResult
    func promoteStandby(callId: String, transport: String, token: String, expectEpoch: UInt64, nonce: String) async throws -> V2PromoteStandbyResponse {
        standbyPromoteCalls.append((transport, token, expectEpoch))
        if standbyPromoteArmed || standbyPromoteParkOnCall == standbyPromoteCalls.count {
            standbyPromoteParkOnCall = nil
            return try await withCheckedThrowingContinuation { cont in
                self.standbyPromoteContinuation = cont
            }
        }
        if !standbyPromoteScript.isEmpty {
            switch standbyPromoteScript.removeFirst() {
            case .success(let response): return response
            case .failure(let error): throw error
            }
        }
        if let standbyPromoteError { throw standbyPromoteError }
        return standbyPromoteResult
    }
    private(set) var mediaStateCalls = 0
    /// Build-51: parks the mediaState call until resumeStandbyState(): an
    /// in-flight reconcile a dying lifecycle can supersede mid-await.
    var standbyStateArmed = false
    private var standbyStateContinuation: CheckedContinuation<V2MediaStateResponse, Error>?
    func resumeStandbyState() {
        standbyStateArmed = false
        guard let cont = standbyStateContinuation else { return }
        standbyStateContinuation = nil
        if let standbyStateError {
            cont.resume(throwing: standbyStateError)
        } else {
            cont.resume(returning: standbyStateResult)
        }
    }
    func mediaState(callId: String) async throws -> V2MediaStateResponse {
        mediaStateCalls += 1
        if standbyStateArmed {
            return try await withCheckedThrowingContinuation { cont in
                self.standbyStateContinuation = cont
            }
        }
        if let standbyStateError { throw standbyStateError }
        return standbyStateResult
    }
    func discardMediaProbe(callId: String) async throws {
        discardProbeCalls.append(callId)
    }
    // MARK: Device-scoped idle preflight (RoutePreflightController tests)

    var preflightIce = ICEConfiguration(
        policy: "all", iceServers: [], expiresAt: "2026-01-01T00:00:00Z",
        mediaTransports: ["ice", "ws"])
    var preflightIceError: Error?
    var preflightAnswer = V2PreflightAnswer(
        sdp: "v=0\r\n", type: "answer", iceMode: "stun", preflightId: "prb_1", ttlMs: 45_000)
    var preflightAttachError: Error?
    var preflightAttachCount = 0
    private(set) var renewPreflightIds: [String] = []
    var preflightRenewError: Error?
    var preflightRenewTTLMs: Int64 = 45_000
    /// When armed once, the next renewal parks until resumeRenew (in-flight
    /// renewal race tests).
    private var renewArmed = false
    private var renewContinuation: CheckedContinuation<Void, Error>?

    func armRenewWait() { renewArmed = true }
    func resumeRenew(with result: Result<Void, Error>) {
        renewContinuation?.resume(with: result)
        renewContinuation = nil
    }

    /// Real fetch evidence for the bounded ICE-config cache tests.
    var preflightIceFetchCount = 0
    /// Build-68 incremental exchange markers.
    var preflightTrickleAttaches = 0
    var preflightCandidatePosts: [[String]] = []
    var preflightCandidatePolls = 0

    func iceConfiguration() async throws -> ICEConfiguration {
        preflightIceFetchCount += 1
        if let preflightIceError { throw preflightIceError }
        return preflightIce
    }
    func attachMediaPreflight(sdp: String) async throws -> V2PreflightAnswer {
        preflightAttachCount += 1
        if let preflightAttachError { throw preflightAttachError }
        return preflightAnswer
    }
    func discardMediaPreflight(preflightId: String) async throws {
        discardPreflightIds.append(preflightId)
    }
    func renewMediaPreflight(preflightId: String) async throws -> V2PreflightRenewAnswer {
        renewPreflightIds.append(preflightId)
        if renewArmed {
            renewArmed = false
            try await withCheckedThrowingContinuation { renewContinuation = $0 }
        }
        if let preflightRenewError { throw preflightRenewError }
        return V2PreflightRenewAnswer(preflightId: preflightId, ttlMs: preflightRenewTTLMs)
    }
    func deleteThread(threadKey: String) async throws {
        if let error = deleteThreadErrors[threadKey] { throw error }
        if holdDeletes {
            try await withCheckedThrowingContinuation { deleteContinuation = $0 }
            holdDeletes = false
        }
        deletedThreadKeys.append(threadKey)
    }

    /// Build-67 responsiveness tests: park the delete until resumed so the
    /// PENDING (optimistic) window is observable deterministically.
    var holdDeletes = false
    private var deleteContinuation: CheckedContinuation<Void, Error>?
    func armDeleteHold() { holdDeletes = true }
    func resumeDelete(_ result: Result<Void, Error> = .success(())) {
        holdDeletes = false
        deleteContinuation?.resume(with: result)
        deleteContinuation = nil
    }

    // MARK: Build-55 per-key SMS quarantine

    /// When false, folder listing throws notReady/404 like an old gateway so
    /// the client keeps the local classification fallback.
    var quarantineSupported = false
    var quarantineFolders: [String: [MessageThread]] = [:]
    private(set) var restoredThreadKeys: [String] = []

    func listThreads(folder: String) async throws -> [MessageThread] {
        guard quarantineSupported else { throw APIError.notReady("旧网关") }
        return quarantineFolders[folder] ?? []
    }

    func restoreThread(threadKey: String) async throws {
        guard quarantineSupported else { throw APIError.notReady("旧网关") }
        restoredThreadKeys.append(threadKey)
        var junk = quarantineFolders["junk"] ?? []
        if let thread = junk.first(where: { $0.key == threadKey }) {
            junk.removeAll { $0.key == threadKey }
            quarantineFolders["junk"] = junk
            quarantineFolders["normal"] = (quarantineFolders["normal"] ?? []) + [thread]
        }
    }

    // MARK: Build-55 key-scoped harassment policy

    /// When false, every harassment route throws notReady like an old gateway
    /// so the app keeps the local fallback.
    var harassmentSupported = false
    var harassmentCall = false
    var harassmentSMS = false
    var harassmentContacts = false
    var harassmentVersionValue: Int64 = 0
    var harassmentEntryRecords: [HarassmentEntryRecord] = []
    private(set) var harassmentSettingsWrites: [(call: Bool, sms: Bool, contact: Bool?)] = []
    var harassmentAddError: APIError?

    private func harassmentSnapshot() -> HarassmentSettingsRecord {
        HarassmentSettingsRecord(
            callBlocking: harassmentCall, smsBlocking: harassmentSMS,
            contactWhitelist: harassmentContacts, version: harassmentVersionValue, updatedAt: nil)
    }

    // Test gates: suspend the NEXT call so a stale old-pairing response can be
    // released AFTER a reset/pairing switch (session-fence tests).
    var harassmentSettingsSuspendedConfirmed = false
    private var harassmentSettingsSuspended = false
    private var harassmentSettingsContinuation: CheckedContinuation<Void, Never>?

    func suspendNextHarassmentSettings() {
        harassmentSettingsSuspended = true
        harassmentSettingsSuspendedConfirmed = false
    }

    func resumeHarassmentSettings() {
        harassmentSettingsContinuation?.resume()
        harassmentSettingsContinuation = nil
    }

    func harassmentSettings() async throws -> HarassmentSettingsRecord {
        guard harassmentSupported else { throw APIError.notReady("旧网关") }
        if harassmentSettingsSuspended {
            harassmentSettingsSuspended = false
            harassmentSettingsSuspendedConfirmed = true
            await withCheckedContinuation { harassmentSettingsContinuation = $0 }
        }
        return harassmentSnapshot()
    }

    var updateSettingsSuspendedConfirmed = false
    private var updateSettingsSuspended = false
    private var updateSettingsContinuation: CheckedContinuation<Void, Never>?

    func suspendNextHarassmentSettingsUpdate() {
        updateSettingsSuspended = true
        updateSettingsSuspendedConfirmed = false
    }

    func resumeHarassmentSettingsUpdate() {
        updateSettingsContinuation?.resume()
        updateSettingsContinuation = nil
    }

    func updateHarassmentSettings(callBlocking: Bool, smsBlocking: Bool, contactWhitelist: Bool?, version: Int64?) async throws -> HarassmentSettingsRecord {
        guard harassmentSupported else { throw APIError.notReady("旧网关") }
        if updateSettingsSuspended {
            updateSettingsSuspended = false
            updateSettingsSuspendedConfirmed = true
            await withCheckedContinuation { updateSettingsContinuation = $0 }
        }
        if let version, version != harassmentVersionValue {
            throw APIError.http(status: 409, code: "CB-V2-409V", message: nil)
        }
        harassmentCall = callBlocking
        harassmentSMS = smsBlocking
        if let contactWhitelist { harassmentContacts = contactWhitelist }
        harassmentVersionValue += 1
        harassmentSettingsWrites.append((callBlocking, smsBlocking, contactWhitelist))
        return harassmentSnapshot()
    }

    func harassmentEntries() async throws -> [HarassmentEntryRecord] {
        guard harassmentSupported else { throw APIError.notReady("旧网关") }
        return harassmentEntryRecords
    }

    private(set) var harassmentReplaceFlags: [Bool] = []
    var harassmentAddEffective = true
    var harassmentAddNote: String?

    var addEntrySuspendedConfirmed = false
    /// Number of addHarassmentEntry attempts seen (including a suspended one):
    /// proves an import loop aborted instead of writing remaining payloads.
    private(set) var addEntryCalls = 0
    private var addEntrySuspended = false
    private var addEntryContinuation: CheckedContinuation<Void, Never>?

    func suspendNextAddHarassmentEntry() {
        addEntrySuspended = true
        addEntrySuspendedConfirmed = false
    }

    func resumeAddHarassmentEntry() {
        addEntryContinuation?.resume()
        addEntryContinuation = nil
    }

    func addHarassmentEntry(number: String, matchKind: String, mode: String, action: String?, replaceOpposite: Bool) async throws -> HarassmentEntryAddResult {
        guard harassmentSupported else { throw APIError.notReady("旧网关") }
        addEntryCalls += 1
        if addEntrySuspended {
            addEntrySuspended = false
            addEntrySuspendedConfirmed = true
            await withCheckedContinuation { addEntryContinuation = $0 }
        }
        if let harassmentAddError { throw harassmentAddError }
        harassmentReplaceFlags.append(replaceOpposite)
        var replaced = 0
        if replaceOpposite, matchKind == "exact" {
            let keys = Set(PhoneNormalizer.canonicalKeys(number))
            let opposite = mode == "trust" ? "block" : "trust"
            let before = harassmentEntryRecords.count
            harassmentEntryRecords.removeAll { entry in
                entry.mode == opposite && entry.matchKind == "exact"
                    && !keys.isDisjoint(with: PhoneNormalizer.canonicalKeys(entry.number))
            }
            replaced = before - harassmentEntryRecords.count
        }
        let entry = HarassmentEntryRecord(
            id: "hx_\(harassmentEntryRecords.count)", number: number, matchKind: matchKind,
            mode: mode, action: action ?? (mode == "trust" ? "allow" : "reject"),
            source: "manual", listId: nil, enabled: true, createdAt: nil)
        harassmentEntryRecords.append(entry)
        return HarassmentEntryAddResult(
            entry: entry, replaced: replaced,
            effective: harassmentAddEffective, note: harassmentAddNote)
    }

    func deleteHarassmentEntry(id: String) async throws {
        guard harassmentSupported else { throw APIError.notReady("旧网关") }
        harassmentEntryRecords.removeAll { $0.id == id }
    }

    func harassmentLists() async throws -> [HarassmentListRecord] {
        guard harassmentSupported else { throw APIError.notReady("旧网关") }
        return []
    }

    func harassmentHistory() async throws -> [HarassmentEventRecord] {
        guard harassmentSupported else { throw APIError.notReady("旧网关") }
        return []
    }

    // MARK: Recording settings (build-55 session fence + auto preference)

    var recordingAutoModeValue = "all"
    var recordingDeviceAutoRecord = false
    /// Storage state the fake reports (nil = mounted/healthy defaults).
    var recordingStorageValue: RecordingStorageStatusRecord?
    var recordingSettingsSuspendedConfirmed = false
    private var recordingSettingsSuspended = false
    private var recordingSettingsContinuation: CheckedContinuation<Void, Never>?

    func suspendNextRecordingSettings() {
        recordingSettingsSuspended = true
        recordingSettingsSuspendedConfirmed = false
    }

    func resumeRecordingSettings() {
        recordingSettingsContinuation?.resume()
        recordingSettingsContinuation = nil
    }

    func recordingSettings() async throws -> RecordingSettingsRecord {
        if recordingSettingsSuspended {
            recordingSettingsSuspended = false
            recordingSettingsSuspendedConfirmed = true
            await withCheckedContinuation { recordingSettingsContinuation = $0 }
        }
        return RecordingSettingsRecord(
            enabled: true, autoMode: recordingAutoModeValue,
            deviceAutoRecord: recordingDeviceAutoRecord, retentionDays: 0,
            minimumFreeBytes: nil, maxBytes: nil, storage: recordingStorageValue)
    }

    func updateDeviceAutoRecord(_ enabled: Bool) async throws -> Bool {
        if let autoRecordUpdateError { throw autoRecordUpdateError }
        if holdAutoRecordUpdates {
            try await withCheckedThrowingContinuation { autoRecordContinuation = $0 }
            holdAutoRecordUpdates = false
        }
        recordingDeviceAutoRecord = enabled
        return enabled
    }

    /// Build-67 recording-switch tests: park the PATCH so the pending window
    /// (and refreshes racing it) is observable deterministically.
    var autoRecordUpdateError: Error?
    var holdAutoRecordUpdates = false
    private var autoRecordContinuation: CheckedContinuation<Void, Error>?
    func armAutoRecordHold() { holdAutoRecordUpdates = true }
    func resumeAutoRecord(_ result: Result<Void, Error> = .success(())) {
        holdAutoRecordUpdates = false
        autoRecordContinuation?.resume(with: result)
        autoRecordContinuation = nil
    }
    func mediaMeasureWebSocketRequest(callId: String) async throws -> URLRequest {
        measureRequestCallCount += 1
        return URLRequest(url: URL(string: "wss://example.test/calls/\(callId)/media/measure")!)
    }
    func mediaRelayProbeWebSocketRequest() async throws -> URLRequest {
        relayProbeRequestCallCount += 1
        return URLRequest(url: URL(string: "wss://example.test/api/v2/media/relay-probe")!)
    }

    // MARK: QUIC direct (build 61)

    /// Decoded-from-JSON stub surface (QUICMediaInfo has a custom Decodable
    /// and no memberwise initializer).
    var quicInfoResult: Result<QUICMediaInfo, Error> =
        .failure(APIError.notReady("quic not configured"))
    var quicCandidateResult: Result<QUICPunchInfo, Error> =
        .failure(APIError.notReady("quic not configured"))
    private(set) var quicInfoCalls = 0
    private(set) var quicCandidatePosts: [(callId: String, candidate: String, networkEpoch: String)] = []

    /// Build-62: the stub serves the REAL Go-signed pin fixture (see
    /// `QUICPinFixture`) — the production decoder and trust verifier run
    /// against actual gateway serialization, not a hand-written fictional
    /// shape.
    static func makeQUICInfoJSON() -> String {
        QUICPinFixture.validDocument
    }

    func quicMediaInfo() async throws -> QUICMediaInfo {
        quicInfoCalls += 1
        return try quicInfoResult.get()
    }

    func postQUICCandidate(callId: String, candidate: String, networkEpoch: String) async throws -> QUICPunchInfo {
        quicCandidatePosts.append((callId, candidate, networkEpoch))
        return try quicCandidateResult.get()
    }

    // MARK: Conditional history (ETag/304 simulation)
    //
    // Models the REAL gateway semantics: the server computes a strong content
    // ETag per response body and answers `304` ONLY when the CLIENT presented
    // a matching `If-None-Match`. The client's persisted validator is read
    // through `validatorReader` (wire it to the durable history store file to
    // prove cold-restart behavior). With no reader the fake always returns
    // full `.changed` bodies — exactly the pre-cache behavior existing tests
    // relied on. `conditionalNeverRevalidates` models an OLD gateway: full
    // `.changed` with `validator == nil` forever. Counters separate FULL body
    // transfers from `304`s so request-count/byte measurements are asserted.

    var conditionalNeverRevalidates = false
    /// Thrown from EVERY conditional history call before anything else —
    /// models a transient network failure (which must never retire cached
    /// normal/junk rows, unlike a genuine folder-unsupported 404).
    var conditionalListError: Error?
    /// Client-side validator lookup for a canonical request key (normally the
    /// persisted scoped snapshot). nil = the client never sent If-None-Match.
    var validatorReader: ((String) -> String?)?
    private(set) var listThreadsFullBodyCount = 0
    private(set) var listThreads304Count = 0
    private(set) var listCallsFullBodyCount = 0
    private(set) var listCalls304Count = 0
    private(set) var pageFetchFullBodyCount = 0
    private(set) var pageFetch304Count = 0
    /// Every `after` cursor passed to v1 `listMessages(after:)` (bounded
    /// catch-up shape assertions).
    private(set) var listMessagesAfterCursors: [Int64] = []
    var callsStub: [CallRecord] = []

    private func fakeSignature<T: Encodable>(_ value: T) -> String {
        // Deterministic content hash: the real gateway etag is the SHA of its
        // exact response bytes, so the fake must not depend on JSONEncoder's
        // unspecified per-instance key order.
        let encoder = JSONEncoder.iso
        encoder.outputFormatting = [.sortedKeys]
        let data = (try? encoder.encode(value)) ?? Data()
        return "\"" + String(SHA256Lite.hex(data).prefix(16)) + "\""
    }

    private static func fakeValidatorKey(_ path: String, query: [URLQueryItem]) -> String {
        var components = URLComponents(url: URL(string: "https://fake-gateway.test/api/v2/\(path)")!,
                                       resolvingAgainstBaseURL: false)!
        if !query.isEmpty { components.queryItems = query }
        return components.url!.absoluteString
    }

    /// Resolve like the pinned gateway: no client validator → 200 full body;
    /// matching client validator → 304; mismatched → 200 with the new ETag.
    private func conditionalOutcome<T: Encodable>(
        content: T, key: String, full: Int, threeFour: Int,
        bumpFull: WritableKeyPath<FakeGatewayAPI, Int>,
        bump304: WritableKeyPath<FakeGatewayAPI, Int>
    ) -> ConditionalOutcome<T> {
        // (kept for shape parity; the real branch logic lives in each method)
        _ = (key, full, threeFour, bumpFull, bump304)
        if conditionalNeverRevalidates { return .changed(content, validator: nil) }
        let etag = fakeSignature(content)
        if validatorReader?(key) == etag { return .notModified }
        return .changed(content, validator: HistoryValidator(key: key, etag: etag))
    }

    /// Park the NEXT conditional list GET (stale-response and cold-activation
    /// races), mirroring how a real in-flight request spans a rebind.
    private var conditionalListArmed = false
    private var conditionalListContinuation: CheckedContinuation<ConditionalOutcome<[MessageThread]>, Error>?
    func armConditionalListWait() { conditionalListArmed = true }
    var conditionalListParked: Bool { conditionalListContinuation != nil }
    func resumeConditionalList(with result: Result<ConditionalOutcome<[MessageThread]>, Error>) {
        conditionalListArmed = false
        conditionalListContinuation?.resume(with: result)
        conditionalListContinuation = nil
    }

    func listThreadsConditional(folder: String?, lineId: String?) async throws -> ConditionalOutcome<[MessageThread]> {
        if let error = conditionalListError { throw error }
        if conditionalListArmed {
            conditionalListArmed = false
            return try await withCheckedThrowingContinuation { conditionalListContinuation = $0 }
        }
        // Reuse the plain surface (keeps quarantine semantics + the classic
        // suspendNextListThreads park used by the build-67 fence tests).
        let content: [MessageThread]
        if let folder {
            content = try await listThreads(folder: folder)
        } else {
            content = try await listThreads(lineId: lineId)
        }
        var query: [URLQueryItem] = []
        if let folder { query.append(URLQueryItem(name: "folder", value: folder)) }
        else if let lineId { query.append(URLQueryItem(name: "line", value: lineId)) }
        let key = Self.fakeValidatorKey("threads", query: query)
        if conditionalNeverRevalidates {
            listThreadsFullBodyCount += 1
            return .changed(content, validator: nil)
        }
        let etag = fakeSignature(content)
        if validatorReader?(key) == etag {
            listThreads304Count += 1
            return .notModified
        }
        listThreadsFullBodyCount += 1
        return .changed(content, validator: HistoryValidator(key: key, etag: etag))
    }

    func listCallsConditional(limit: Int) async throws -> ConditionalOutcome<[CallRecord]> {
        if conditionalCallsArmed {
            conditionalCallsArmed = false
            return try await withCheckedThrowingContinuation { conditionalCallsContinuation = $0 }
        }
        let content = callsStub
        let key = Self.fakeValidatorKey("calls", query: [URLQueryItem(name: "limit", value: String(limit))])
        if conditionalNeverRevalidates {
            listCallsFullBodyCount += 1
            return .changed(content, validator: nil)
        }
        let etag = fakeSignature(content)
        if validatorReader?(key) == etag {
            listCalls304Count += 1
            return .notModified
        }
        listCallsFullBodyCount += 1
        return .changed(content, validator: HistoryValidator(key: key, etag: etag))
    }

    private var conditionalCallsArmed = false
    private var conditionalCallsContinuation: CheckedContinuation<ConditionalOutcome<[CallRecord]>, Error>?
    func armConditionalCallsWait() { conditionalCallsArmed = true }
    var conditionalCallsParked: Bool { conditionalCallsContinuation != nil }
    func resumeConditionalCalls(with result: Result<ConditionalOutcome<[CallRecord]>, Error>) {
        conditionalCallsArmed = false
        conditionalCallsContinuation?.resume(with: result)
        conditionalCallsContinuation = nil
    }

    func listThreadMessagesConditional(
        threadKey: String, beforeCreatedAt: Int64?, beforeID: String?, limit: Int
    ) async throws -> ConditionalOutcome<ThreadMessagePage> {
        requestedThreadKeys.append(threadKey)
        if let error = threadPageErrors[threadKey] { throw error }
        let content = ThreadMessagePage(messages: threadPages[threadKey] ?? [], hasMore: threadHasMore)
        var query = [URLQueryItem(name: "threadKey", value: threadKey),
                     URLQueryItem(name: "limit", value: String(limit))]
        if let beforeCreatedAt { query.append(URLQueryItem(name: "before", value: String(beforeCreatedAt))) }
        if let beforeID { query.append(URLQueryItem(name: "beforeId", value: beforeID)) }
        let key = Self.fakeValidatorKey("messages", query: query)
        if conditionalNeverRevalidates {
            pageFetchFullBodyCount += 1
            return .changed(content, validator: nil)
        }
        let etag = fakeSignature(content)
        if validatorReader?(key) == etag {
            pageFetch304Count += 1
            return .notModified
        }
        pageFetchFullBodyCount += 1
        return .changed(content, validator: HistoryValidator(key: key, etag: etag))
    }

    /// Test seam: simulate the server-side state of ANOTHER device having
    /// read a thread's unreads (NO event is broadcast for this), so the next
    /// bounded conditional revalidation MUST return a 200 with fresh content.
    func simulateRemoteReadOfThread(_ key: String) {
        if let index = threads.firstIndex(where: { $0.key == key }) {
            threads[index].unreadCount = 0
        }
        for folder in ["normal", "junk"] {
            if var list = quarantineFolders[folder],
               let index = list.firstIndex(where: { $0.key == key }) {
                list[index].unreadCount = 0
                quarantineFolders[folder] = list
            }
        }
    }
}

// MARK: - Async pumping

@MainActor
func pumpMainActor(_ times: Int = 5) async {
    for _ in 0..<times {
        await Task.yield()
        try? await Task.sleep(nanoseconds: 5_000_000)
    }
}

@MainActor
func waitUntil(timeout: TimeInterval = 3, _ condition: @MainActor () -> Bool) async {
    let start = Date()
    while Date().timeIntervalSince(start) < timeout {
        await Task.yield()
        if condition() { return }
        try? await Task.sleep(nanoseconds: 20_000_000)
    }
}

func makeCallRecord(
    id: String,
    state: CallState,
    direction: CallDirection = .outbound,
    peer: String = "555-0123",
    startedAt: Int64? = nil
) -> CallRecord {
    let now = Date().unixMilliseconds
    return CallRecord(
        id: id, gatewayID: "gw", lineID: "gw:line", direction: direction, peer: peer,
        state: state, startedAt: startedAt ?? now - 1000,
        connectedAt: state == .active ? now : nil, endedAt: nil, endReason: nil,
        recordingId: nil, recordingState: nil, recordingDurationMs: nil
    )
}

func makeConferenceRecord(id: String = "conf-1", legs: [CallRecord]) -> ConferenceRecord {
    ConferenceRecord(
        id: id, hostDeviceId: "device-1", state: "active",
        createdAt: Date().unixMilliseconds, graceDeadline: nil, legs: legs
    )
}

/// Build-68 test double for the OPTIONAL incremental capability: everything
/// else is inherited from `FakeGatewayAPI`, which is the deliberate pre-68
/// conformer. Tests that need the incremental path use THIS type; tests that
/// exercise backward compatibility keep using the base fake.
final class FakeTrickleGatewayAPI: FakeGatewayAPI, TrickleMediaAPI {
    func attachMediaProbe(callId: String, sdp: String, trickle: Bool) async throws -> WebRTCAnswer {
        try await attachMediaProbe(callId: callId, sdp: sdp)
    }

    func attachMediaPreflight(sdp: String, trickle: Bool) async throws -> V2PreflightAnswer {
        guard trickle else { return try await attachMediaPreflight(sdp: sdp) }
        preflightAttachCount += 1
        preflightTrickleAttaches += 1
        if let preflightAttachError { throw preflightAttachError }
        return preflightAnswer
    }

    func postCallCandidates(callId: String, attachId: String, candidates: [String]) async throws {}
    func postProbeCandidates(callId: String, attachId: String, candidates: [String]) async throws {}
    func postPreflightCandidates(preflightId: String, attachId: String, candidates: [String]) async throws {
        preflightCandidatePosts.append(candidates)
    }
    func pollCallCandidates(callId: String, attachId: String, after: Int) async throws -> V2CandidateBatch {
        V2CandidateBatch(candidates: [], cursor: after, complete: true, truncated: nil)
    }
    func pollProbeCandidates(callId: String, attachId: String, after: Int) async throws -> V2CandidateBatch {
        V2CandidateBatch(candidates: [], cursor: after, complete: true, truncated: nil)
    }
    func pollPreflightCandidates(preflightId: String, attachId: String, after: Int) async throws -> V2CandidateBatch {
        preflightCandidatePolls += 1
        return V2CandidateBatch(candidates: [], cursor: after, complete: true, truncated: nil)
    }
}
