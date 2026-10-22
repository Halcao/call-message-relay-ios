import Foundation

/// Reserved fake identifiers for the isolated demo mode. They are drawn from
/// documentation/example ranges and the explicit `.demo` / `demo.` prefixes so
/// they can never collide with a real gateway id or dialable number.
enum DemoConstants {
    static let gatewayId = "demo.gateway.callrelay.local"
    static let gatewayName = "演示网关（离线模拟）"
    static let endpoint = "demo://in-memory"
    static let fingerprint = "DEMO-FINGERPRINT-NOT-A-REAL-KEY"
    static let transport = "tailnet"
    /// Fictional 555 range used in examples; never sent to a modem.
    static let selfPeer = "555-0100"
    static let demoPeers = ["555-0123", "555-0148", "555-0177"]
}

/// Launch arguments used by UI tests to start directly in the fully offline
/// demo without onboarding or stored defaults.
enum LaunchArguments {
    static let forceDemo = "-callrelayDemoMode"
    /// UI-test support: populate the in-memory contact snapshot with synthetic
    /// demo contacts (Chinese/Latin names, 555 numbers) so recipient
    /// autocomplete is provable without system Contacts authorization.
    static let demoContacts = "-callrelayDemoContacts"
    /// Hermetic UI-test support flags.
    static let uiTestReset = "-callrelayUITestReset"
    static let demoEnableSpamPresets = "-callrelayDemoSpamPresets"
    /// Screenshot-only preview: functional offline demo driver plus three
    /// synthetic unified lines exercising the default/per-call picker.
    static let multilinePreview = "-callrelayMultilinePreview"
    static let showLineChooser = "-callrelayShowLineChooser"
    static let showNumberEditor = "-callrelayShowNumberEditor"
    /// Screenshot-only: renders the *live paired-mode* line surfaces from the
    /// same synthetic lines without any network or credentials. Optional
    /// `authLostFixture` additionally renders the definitive-auth-lost prompt.
    static let pairedFixture = "-callrelayPairedFixture"
    static let authLostFixture = "-callrelayAuthLostFixture"
    /// Screenshot-only: clears the synthetic line's signal report so the
    /// dialer renders the honest unknown/no-bars state.
    static let unknownSignalPreview = "-callrelayUnknownSignalPreview"
    /// Screenshot-only: renders the contact import/merge preview from a purely
    /// synthetic fixture. Never reads or writes the real address book.
    static let contactImportFixture = "-callrelayContactImportFixture"
    /// Screenshot-only: deterministic demo active call + synthetic route
    /// state so the in-call Auto/Direct/Relay menu renders without a gateway.
    static let routePreview = "-callrelayRoutePreview"
    /// Screenshot-only: deterministic Settings route picker (no active call).
    static let routeSettingsPreview = "-callrelayRouteSettingsPreview"
    /// Screenshot-only: renders 0..4 signal-bar states in one strip.
    static let signalBarsPreview = "-callrelaySignalBarsPreview"
    /// Screenshot-only: deterministic demo active call already recording plus
    /// synthetic 张三/李四 recordings for the in-call indicator and the
    /// RecordingsView list. Offline, no credentials, no real audio.
    static let recordingPreview = "-callrelayRecordingPreview"
    /// Screenshot-only: the recordings LIST fixture without an active call
    /// (the in-call cover would otherwise hide the tab bar).
    static let recordingListPreview = "-callrelayRecordingListPreview"
    /// Screenshot-only: deterministic demo active conference with its own
    /// manual recording state (host leg + synthetic 张三/李四 members) so the
    /// in-call conference recording surfaces render offline.
    static let conferenceRecordingPreview = "-callrelayConferenceRecordingPreview"
    /// Screenshot-only: a synthetic harassment-CAPABLE gateway so the
    /// supported branch (gateway list/history/import) renders offline.
    static let harassmentGatewayPreview = "-callrelayHarassmentGatewayPreview"
    /// Screenshot-only: the native gateway-administrator console rendered from
    /// a purely synthetic signed-in session (lines/keys/devices/calls/
    /// voicemails/recording/settings/diagnostics). Never contacts a gateway,
    /// never logs in, no real credentials.
    static let managementPreview = "-callrelayManagementPreview"
    /// DEBUG-only: the same paired fixture WITHOUT a seeded session, so the
    /// administrator login renders with a real (synthetic) gateway origin —
    /// enabled fields and the session-expiry recovery state — never a dead
    /// form. No password, no network.
    static let managementLoginPreview = "-callrelayManagementLoginPreview"

    static var isRoutePreview: Bool {
        ProcessInfo.processInfo.arguments.contains(routePreview)
    }
    static var isRouteSettingsPreview: Bool {
        ProcessInfo.processInfo.arguments.contains(routeSettingsPreview)
    }
    static var isSignalBarsPreview: Bool {
        ProcessInfo.processInfo.arguments.contains(signalBarsPreview)
    }
    static var isRecordingPreview: Bool {
        ProcessInfo.processInfo.arguments.contains(recordingPreview)
    }
    static var isRecordingListPreview: Bool {
        ProcessInfo.processInfo.arguments.contains(recordingListPreview)
    }
    static var isConferenceRecordingPreview: Bool {
        ProcessInfo.processInfo.arguments.contains(conferenceRecordingPreview)
    }
    static var isHarassmentGatewayPreview: Bool {
        ProcessInfo.processInfo.arguments.contains(harassmentGatewayPreview)
    }
    static var isManagementLoginPreview: Bool {
        #if DEBUG
        ProcessInfo.processInfo.arguments.contains(managementLoginPreview)
        #else
        false
        #endif
    }

    static var isManagementPreview: Bool {
        #if DEBUG
        ProcessInfo.processInfo.arguments.contains(managementPreview)
        #else
        // Release builds have no screenshot fixture: the flag is inert and
        // the seeding code is not compiled at all.
        false
        #endif
    }

    static var isUITestReset: Bool {
        ProcessInfo.processInfo.arguments.contains(uiTestReset)
    }
    static var enablesDemoSpamPresets: Bool {
        ProcessInfo.processInfo.arguments.contains(demoEnableSpamPresets)
    }
    static var enablesDemoContacts: Bool {
        ProcessInfo.processInfo.arguments.contains(demoContacts)
    }
    static var enablesMultilinePreview: Bool {
        ProcessInfo.processInfo.arguments.contains(multilinePreview)
    }
    static var showsUnknownSignal: Bool {
        ProcessInfo.processInfo.arguments.contains(unknownSignalPreview)
    }
    static var isContactImportFixture: Bool {
        ProcessInfo.processInfo.arguments.contains(contactImportFixture)
    }
}
