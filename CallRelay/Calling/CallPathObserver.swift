import Foundation
import Network

/// Build-66: one comparable signature of the OS network path. The signature
/// is deliberately COARSE (reachability, the USED route families, coarse
/// kernel interface names of the used families, expensive / constrained
/// flags) — never SSIDs, addresses or any user location data.
///
/// Review correction: `NWPath.availableInterfaces` is NOT the selected
/// route — Wi-Fi and cellular are both usually "available" on a phone, so a
/// signature built from the available set would miss a Wi-Fi → cellular
/// route switch entirely. The used families come from
/// `NWPath.usesInterfaceType`; the coarse interface names ("en0",
/// "pdp_ip0", never addresses) catch a same-family switch onto a different
/// interface.
struct CallPathSignature: Equatable, Sendable {
    enum InterfaceKind: String, Hashable, Sendable, CaseIterable {
        case wifi, cellular, wired, other
    }

    let satisfied: Bool
    /// Route families this path actually USES (not what is available).
    let usedFamilies: Set<InterfaceKind>
    /// Coarse interface identity: kernel names of the available interfaces
    /// whose family is one of `usedFamilies`. Names only — never addresses.
    let interfaceNames: Set<String>
    let isExpensive: Bool
    let isConstrained: Bool

    init(satisfied: Bool,
         usedFamilies: Set<InterfaceKind>,
         interfaceNames: Set<String> = [],
         isExpensive: Bool = false,
         isConstrained: Bool = false) {
        self.satisfied = satisfied
        self.usedFamilies = usedFamilies
        self.interfaceNames = interfaceNames
        self.isExpensive = isExpensive
        self.isConstrained = isConstrained
    }

    init(path: NWPath) {
        var used = Set<InterfaceKind>()
        if path.usesInterfaceType(.wifi) { used.insert(.wifi) }
        if path.usesInterfaceType(.cellular) { used.insert(.cellular) }
        if path.usesInterfaceType(.wiredEthernet) { used.insert(.wired) }
        if path.usesInterfaceType(.other) { used.insert(.other) }
        var names = Set<String>()
        for interface in path.availableInterfaces {
            let kind: InterfaceKind
            switch interface.type {
            case .wifi: kind = .wifi
            case .cellular: kind = .cellular
            case .wiredEthernet: kind = .wired
            default: kind = .other
            }
            if used.contains(kind) { names.insert(interface.name) }
        }
        self.init(satisfied: path.status == .satisfied,
                  usedFamilies: used,
                  interfaceNames: names,
                  isExpensive: path.isExpensive,
                  isConstrained: path.isConstrained)
    }

    /// Privacy-safe diagnostics rendering (family KINDS only — interface
    /// names stay inside the equality comparison, never in logs).
    var debugDescription: String {
        let kinds = usedFamilies.map(\.rawValue).sorted().joined(separator: "+")
        return "\(satisfied ? "up" : "down")[\(kinds)]"
            + (isExpensive ? " expensive" : "") + (isConstrained ? " constrained" : "")
    }
}

/// One MEANINGFUL path transition. Generation ids are monotonic and assigned
/// only when the applied signature actually changed, so consumers can fence
/// duplicate/no-op updates away (never an attempt or retry storm).
enum CallPathEvent: Equatable, Sendable {
    /// The FIRST applied snapshot of this observer: identity only. It is
    /// never a cutover — no consumer may revalidate/arm anything on it.
    case baseline(generation: UInt64)
    case becameUnreachable(generation: UInt64)
    case becameReachable(generation: UInt64)
    case pathChanged(generation: UInt64, previous: CallPathSignature)

    var generation: UInt64 {
        switch self {
        case .baseline(let generation): return generation
        case .becameUnreachable(let generation): return generation
        case .becameReachable(let generation): return generation
        case .pathChanged(let generation, _): return generation
        }
    }
}

/// Pure decision layer for the path observer: deduplicates identical
/// snapshots, coalesces rapid flapping inside `debounce` (the LATEST snapshot
/// wins) and applies the net transition exactly once. Deterministic —
/// callers inject the clock, so the debounce/no-hot-loop rules are
/// unit-testable without a real network.
struct CallPathTracker {
    /// Minimum interval between two APPLIED generations. Rapid flaps inside
    /// one window collapse into a single net transition.
    var debounce: TimeInterval

    private(set) var generation: UInt64 = 0
    /// The last APPLIED signature (nil before the first snapshot).
    private(set) var current: CallPathSignature?
    private var lastAppliedAt: Date?
    private var pending: CallPathSignature?

    init(debounce: TimeInterval = 0.5) {
        self.debounce = debounce
    }

    var isReachable: Bool { current?.satisfied ?? true }

    /// Feeds one raw snapshot. Returns the events to emit NOW (empty when the
    /// snapshot was a duplicate or is still inside the debounce window — a
    /// later `flushDue` then applies the settled snapshot). The FIRST applied
    /// snapshot emits `.baseline` (identity only, never a cutover).
    mutating func ingest(_ signature: CallPathSignature, at now: Date = Date()) -> [CallPathEvent] {
        guard signature != current else {
            // The path returned to the APPLIED signature: any queued flap
            // (a different pending snapshot) is superseded — dropping it is
            // mandatory, or a stale B would be applied after the path
            // settled back to A (review finding: A → B → A inside the
            // debounce window).
            pending = nil
            return []
        }
        if let appliedAt = lastAppliedAt,
           now.timeIntervalSince(appliedAt) < debounce {
            // Inside the window: coalesce (the latest snapshot replaces the
            // pending one) — a flap storm can never emit more than one net
            // transition per window.
            pending = signature
            return []
        }
        return apply(signature, at: now)
    }

    /// Applies the coalesced pending snapshot once its debounce window
    /// elapsed (the observer schedules this; deterministic tests call it
    /// directly). Empty when nothing is pending or the window is still open.
    mutating func flushDue(at now: Date = Date()) -> [CallPathEvent] {
        guard let pending, let appliedAt = lastAppliedAt else { return [] }
        guard now.timeIntervalSince(appliedAt) >= debounce else { return [] }
        return apply(pending, at: now)
    }

    /// When the pending snapshot becomes applicable (nil when none is
    /// pending) — the observer's flush scheduling anchor.
    var nextFlushAt: Date? {
        guard let pending, let appliedAt = lastAppliedAt else { return nil }
        _ = pending
        return appliedAt.addingTimeInterval(debounce)
    }

    private mutating func apply(_ signature: CallPathSignature, at now: Date) -> [CallPathEvent] {
        let previous = current
        if !signature.satisfied, let previous, !previous.satisfied {
            // Interface churn while the path stays unusable is not a new
            // transition: identity, window and generation are unchanged and
            // nothing is emitted.
            pending = nil
            return []
        }
        pending = nil
        current = signature
        lastAppliedAt = now
        generation &+= 1
        let generation = self.generation
        guard let previous else {
            // FIRST applied snapshot: identity only. Seeding the tracker
            // must never look like a transition (a call starting on a
            // healthy path would otherwise spuriously "revalidate" at
            // birth).
            return [.baseline(generation: generation)]
        }
        guard signature.satisfied else {
            return [.becameUnreachable(generation: generation)]
        }
        guard previous.satisfied else {
            return [.becameReachable(generation: generation)]
        }
        return [.pathChanged(generation: generation, previous: previous)]
    }
}

/// The observer seam the call coordinator talks to. Events are delivered on
/// the main actor, and every meaningful transition carries a monotonic
/// generation id that fences stale deliveries.
@MainActor
protocol CallPathObserving: AnyObject {
    var onEvent: ((CallPathEvent) -> Void)? { get set }
    /// The generation of the last APPLIED signature (0 before the first).
    var currentGeneration: UInt64 { get }
    var isReachable: Bool { get }
    func start()
    func stop()
}

/// Production observer: an `NWPathMonitor` feeding the pure tracker.
@MainActor
final class NetworkPathObserver: CallPathObserving {
    var onEvent: ((CallPathEvent) -> Void)?

    private let monitor = NWPathMonitor()
    private let queue = DispatchQueue(label: "callrelay.callpath")
    private var tracker: CallPathTracker
    private var started = false
    private var flushTask: Task<Void, Never>?

    init(debounce: TimeInterval = 0.5) {
        self.tracker = CallPathTracker(debounce: debounce)
    }

    var currentGeneration: UInt64 { tracker.generation }
    var isReachable: Bool { tracker.isReachable }

    func start() {
        guard !started else { return }
        started = true
        monitor.pathUpdateHandler = { [weak self] path in
            let signature = CallPathSignature(path: path)
            Task { @MainActor [weak self] in
                self?.ingest(signature)
            }
        }
        monitor.start(queue: queue)
    }

    func stop() {
        guard started else { return }
        started = false
        monitor.cancel()
        flushTask?.cancel()
        flushTask = nil
    }

    private func ingest(_ signature: CallPathSignature, at now: Date = Date()) {
        for event in tracker.ingest(signature, at: now) {
            DiagnosticsStore.shared.log("call",
                "network path \(signature.debugDescription) gen=\(tracker.generation)")
            onEvent?(event)
        }
        scheduleFlush()
    }

    private func scheduleFlush() {
        flushTask?.cancel()
        flushTask = nil
        guard let due = tracker.nextFlushAt else { return }
        let wait = max(0, due.timeIntervalSinceNow)
        flushTask = Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(wait * 1_000_000_000))
            guard let self, !Task.isCancelled else { return }
            self.flush()
        }
    }

    private func flush() {
        for event in tracker.flushDue() {
            DiagnosticsStore.shared.log("call",
                "network path settled gen=\(tracker.generation)")
            onEvent?(event)
        }
        scheduleFlush()
    }
}
