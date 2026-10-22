import XCTest
@testable import CallRelay

/// Build-66 pure path-identity tests: Wi-Fi ↔ cellular and down/up edges
/// emit exactly one meaningful, monotonic generation; duplicates and rapid
/// flaps can never produce an event storm, and the first snapshot is a
/// BASELINE, never a cutover.
final class CallPathObserverTests: XCTestCase {
    private func wifi(_ satisfied: Bool = true, expensive: Bool = false) -> CallPathSignature {
        CallPathSignature(satisfied: satisfied, usedFamilies: [.wifi], isExpensive: expensive)
    }

    private func cellular(_ satisfied: Bool = true, expensive: Bool = true) -> CallPathSignature {
        CallPathSignature(satisfied: satisfied, usedFamilies: [.cellular], isExpensive: expensive)
    }

    private func wifiAndCellularUsed() -> CallPathSignature {
        CallPathSignature(satisfied: true, usedFamilies: [.wifi, .cellular], isExpensive: false)
    }

    func testFirstSnapshotEmitsBaselineOnly() {
        var tracker = CallPathTracker(debounce: 0.5)
        let events = tracker.ingest(wifi(), at: Date(timeIntervalSince1970: 100))
        XCTAssertEqual(events, [.baseline(generation: 1)])
        XCTAssertEqual(tracker.generation, 1)
        XCTAssertTrue(tracker.isReachable)
        XCTAssertEqual(tracker.current, wifi())
        // The baseline never looks like a transition even when repeated.
        XCTAssertEqual(tracker.ingest(wifi(), at: Date(timeIntervalSince1970: 200)), [])
        XCTAssertEqual(tracker.generation, 1)
    }

    func testBaselineDownStateIsStillIdentityOnly() {
        var tracker = CallPathTracker(debounce: 0.5)
        let events = tracker.ingest(wifi(false), at: Date(timeIntervalSince1970: 100))
        XCTAssertEqual(events, [.baseline(generation: 1)])
        XCTAssertFalse(tracker.isReachable)
        // The first REAL transition is then the up edge.
        XCTAssertEqual(tracker.ingest(wifi(), at: Date(timeIntervalSince1970: 101)),
                       [.becameReachable(generation: 2)])
    }

    func testWiFiToCellularEmitsOneNewGeneration() {
        var tracker = CallPathTracker(debounce: 0.5)
        let t0 = Date(timeIntervalSince1970: 100)
        _ = tracker.ingest(wifi(), at: t0)
        let events = tracker.ingest(cellular(), at: t0.addingTimeInterval(1))
        XCTAssertEqual(events, [.pathChanged(generation: 2, previous: wifi())])
        XCTAssertEqual(tracker.generation, 2)
    }

    func testCellularToWiFiReverseChangeEmitsOneNewGeneration() {
        var tracker = CallPathTracker(debounce: 0.5)
        let t0 = Date(timeIntervalSince1970: 100)
        _ = tracker.ingest(cellular(), at: t0)
        let events = tracker.ingest(wifi(), at: t0.addingTimeInterval(1))
        XCTAssertEqual(events, [.pathChanged(generation: 2, previous: cellular())])
    }

    func testDownEdgeThenUpEdgeEmitDistinctGenerations() {
        var tracker = CallPathTracker(debounce: 0.5)
        let t0 = Date(timeIntervalSince1970: 100)
        _ = tracker.ingest(wifi(), at: t0)
        XCTAssertEqual(tracker.ingest(wifi(false), at: t0.addingTimeInterval(1)),
                       [.becameUnreachable(generation: 2)])
        XCTAssertFalse(tracker.isReachable)
        // Further down signatures (interface churn while unusable) are not
        // new transitions — no generation, no event.
        XCTAssertEqual(tracker.ingest(cellular(false), at: t0.addingTimeInterval(2)), [])
        XCTAssertEqual(tracker.generation, 2)
        // The up edge is one fresh generation.
        XCTAssertEqual(tracker.ingest(cellular(), at: t0.addingTimeInterval(3)),
                       [.becameReachable(generation: 3)])
        XCTAssertTrue(tracker.isReachable)
    }

    func testDuplicateSnapshotsNeverEmitOrBumpGeneration() {
        var tracker = CallPathTracker(debounce: 0.5)
        let t0 = Date(timeIntervalSince1970: 100)
        _ = tracker.ingest(wifi(), at: t0)
        for index in 1...20 {
            XCTAssertEqual(tracker.ingest(wifi(), at: t0.addingTimeInterval(Double(index))), [])
        }
        XCTAssertEqual(tracker.generation, 1)
    }

    func testDebounceCoalescesRapidFlapsIntoOneSettledTransition() {
        var tracker = CallPathTracker(debounce: 1.0)
        let t0 = Date(timeIntervalSince1970: 100)
        _ = tracker.ingest(wifi(), at: t0)
        // Flap inside the window: wifi → cellular → both → cellular.
        XCTAssertEqual(tracker.ingest(cellular(), at: t0.addingTimeInterval(0.1)), [])
        XCTAssertEqual(tracker.ingest(wifiAndCellularUsed(), at: t0.addingTimeInterval(0.2)), [])
        XCTAssertEqual(tracker.ingest(cellular(), at: t0.addingTimeInterval(0.3)), [])
        XCTAssertEqual(tracker.generation, 1, "flaps inside the window must not emit")
        // Too early: the window is still open.
        XCTAssertEqual(tracker.flushDue(at: t0.addingTimeInterval(0.9)), [])
        // Settled: exactly ONE transition describing the net result.
        XCTAssertEqual(tracker.flushDue(at: t0.addingTimeInterval(1.0)),
                       [.pathChanged(generation: 2, previous: wifi())])
        XCTAssertEqual(tracker.current, cellular())
        // Flushing again is a no-op.
        XCTAssertEqual(tracker.flushDue(at: t0.addingTimeInterval(2.0)), [])
    }

    /// Coordinator review regression: A applied → B pending → A returned
    /// inside the debounce window. The queued B is SUPERSEDED and must be
    /// dropped, or a later flush would apply a snapshot the path no longer
    /// has.
    func testFlapBackToCurrentInsideDebounceDropsTheQueuedPending() {
        var tracker = CallPathTracker(debounce: 1.0)
        let t0 = Date(timeIntervalSince1970: 100)
        _ = tracker.ingest(wifi(), at: t0)                                          // A applied
        XCTAssertEqual(tracker.ingest(cellular(), at: t0.addingTimeInterval(0.1)), [])  // B pending
        XCTAssertEqual(tracker.ingest(wifi(), at: t0.addingTimeInterval(0.2)), [])      // back to A
        // The stale B must never be applied later.
        XCTAssertEqual(tracker.flushDue(at: t0.addingTimeInterval(5)), [])
        XCTAssertEqual(tracker.generation, 1)
        XCTAssertEqual(tracker.current, wifi())
        XCTAssertNil(tracker.nextFlushAt)
    }

    /// Coordinator review regression: Wi-Fi and cellular are BOTH available
    /// on a phone; the signature must distinguish the USED route, not the
    /// available set. Same available set, different used family ⇒ a real
    /// transition.
    func testSignatureSeparatesAvailableFamiliesFromUsedRoute() {
        let wifiRoute = CallPathSignature(
            satisfied: true, usedFamilies: [.wifi], interfaceNames: ["en0"])
        let cellularRoute = CallPathSignature(
            satisfied: true, usedFamilies: [.cellular], interfaceNames: ["pdp_ip0"],
            isExpensive: true)
        XCTAssertNotEqual(wifiRoute, cellularRoute)
        var tracker = CallPathTracker(debounce: 0.1)
        let t0 = Date(timeIntervalSince1970: 100)
        XCTAssertEqual(tracker.ingest(wifiRoute, at: t0), [.baseline(generation: 1)])
        XCTAssertEqual(tracker.ingest(cellularRoute, at: t0.addingTimeInterval(1)),
                       [.pathChanged(generation: 2, previous: wifiRoute)])
    }

    /// A same-family switch onto a different interface is still a
    /// transition (coarse kernel names only — never addresses).
    func testSameFamilyInterfaceIdentityChangeIsATransition() {
        let first = CallPathSignature(satisfied: true, usedFamilies: [.wifi],
                                      interfaceNames: ["en0"])
        let second = CallPathSignature(satisfied: true, usedFamilies: [.wifi],
                                       interfaceNames: ["en1"])
        XCTAssertNotEqual(first, second)
    }

    func testGenerationIsStrictlyMonotonicAcrossEdges() {
        var tracker = CallPathTracker(debounce: 0.1)
        let t0 = Date(timeIntervalSince1970: 100)
        var generations: [UInt64] = []
        let sequence: [(CallPathSignature, TimeInterval)] = [
            (wifi(), 0),
            (cellular(), 1),
            (wifi(false), 2),
            (wifi(false), 3),   // no-op
            (wifi(), 4),
            (wifiAndCellularUsed(), 5),
        ]
        for (signature, offset) in sequence {
            for event in tracker.ingest(signature, at: t0.addingTimeInterval(offset)) {
                generations.append(event.generation)
            }
        }
        XCTAssertEqual(generations, [1, 2, 3, 4, 5])
        XCTAssertEqual(generations, generations.sorted())
    }

    func testEventGenerationMatchesTrackerGeneration() {
        var tracker = CallPathTracker(debounce: 0.1)
        let t0 = Date(timeIntervalSince1970: 100)
        for (signature, offset) in [(wifi(), 0.0), (cellular(), 1.0), (wifi(), 2.0)] {
            for event in tracker.ingest(signature, at: t0.addingTimeInterval(offset)) {
                XCTAssertEqual(event.generation, tracker.generation)
            }
        }
    }
}
