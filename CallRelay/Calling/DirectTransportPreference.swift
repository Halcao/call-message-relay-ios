import Foundation
import SwiftUI

/// User-selected direct-transport preference for one gateway (build 61):
/// which transport the AUTO route mode must prefer when the gateway offers
/// both a QUIC DATAGRAM direct path and the existing WebRTC direct path.
///
/// * `auto` — the deterministic ``RouteSelectionPolicy`` scores both direct
///   candidates plus the WSS relay and owns the choice (sustained-gain
///   hysteresis; no transport-type bias).
/// * `quic` — forced QUIC direct (strict; a forced choice is never overridden
///   by scoring while it is media-proven, and an unavailable forced choice
///   keeps the current route rather than silently moving).
/// * `webRTC` — forced existing WebRTC direct (same strictness contract).
enum DirectTransportChoice: String, CaseIterable, Identifiable, Hashable, Sendable {
    case auto
    case quic
    case webRTC

    var id: String { rawValue }

    /// User-facing name (localized through the string catalog), mirroring
    /// `MediaRouteMode.title`.
    var title: String {
        switch self {
        case .auto: return String(localized: "自动")
        case .quic: return String(localized: "QUIC 直连")
        case .webRTC: return String(localized: "现有直连")
        }
    }

    /// Short label for compact status surfaces.
    var shortLabel: String {
        switch self {
        case .auto: return String(localized: "自动")
        case .quic: return String(localized: "QUIC")
        case .webRTC: return String(localized: "现有直连")
        }
    }
}

/// Per-gateway persisted direct-transport preference, mirroring
/// ``MediaRoutePreferenceStore``: keys are scoped by the gateway identifier
/// and `auto` removes the key (it is the default).
@MainActor
final class DirectTransportPreferenceStore {
    static let shared = DirectTransportPreferenceStore()
    private let defaults: UserDefaults
    private let prefix = "directTransport:"

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    func choice(for gatewayID: String?) -> DirectTransportChoice {
        guard let gatewayID, !gatewayID.isEmpty,
              let raw = defaults.string(forKey: prefix + gatewayID),
              let choice = DirectTransportChoice(rawValue: raw) else { return .auto }
        return choice
    }

    func setChoice(_ choice: DirectTransportChoice, for gatewayID: String?) {
        guard let gatewayID, !gatewayID.isEmpty else { return }
        if choice == .auto {
            defaults.removeObject(forKey: prefix + gatewayID)
        } else {
            defaults.set(choice.rawValue, forKey: prefix + gatewayID)
        }
    }
}

extension ICEConfiguration {
    /// True when the gateway's ICE capability advertises the QUIC DATAGRAM
    /// direct audio transport — currently the `"quic"` token in the v2
    /// `mediaTransports` list (older gateways never list it, so this stays
    /// false and every new UI stays hidden). SEAM: when the gateway grows a
    /// first-class `mediaQuic` capability flag, derive this from it instead.
    var advertisesQUICMedia: Bool {
        mediaTransports?.contains("quic") == true
    }
}

extension RouteCandidateKind {
    /// User-facing name for the 当前路径 status rows (localized through the
    /// string catalog; the relay label is shared with the rest of the UI).
    var title: String {
        switch self {
        case .quicDirect: return String(localized: "QUIC 直连")
        case .webRTCDirect: return String(localized: "现有直连")
        case .wssRelay: return String(localized: "中继")
        }
    }
}
