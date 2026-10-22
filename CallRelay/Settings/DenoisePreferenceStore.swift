import Foundation

/// DenoiseChoice is the resolved directional policy attached to media
/// requests (uplink = local mic, downlink = remote/modem leg).
struct DenoiseChoice: Equatable, Sendable {
    var uplink: Bool
    var downlink: Bool
}

/// DenoisePreferenceStore holds the two independent directional voice-denoise
/// switches per gateway (build 48). Local persistence (UserDefaults, keyed by
/// gateway id) drives the UI and the attach parameters; the same choice is
/// synced to the gateway's per-device `preferences` endpoint so the daemon's
/// SpeexDSP preprocessors apply the policy.
///
/// Isolation: keys are gateway-scoped and the server persists per DEVICE, so
/// two phones (or a PWA) never share a switch. Build-49 honesty policy: a
/// value never set locally or on the server means OFF (the build-48 light-on
/// default was gated on authorized human-speech quality + realtime evidence,
/// which could not be established; both switches stay fully usable and an
/// explicit choice always wins).
@MainActor
final class DenoisePreferenceStore {
    static let shared = DenoisePreferenceStore()

    private let defaults: UserDefaults
    private init(defaults: UserDefaults = .standard) { self.defaults = defaults }

    private static func key(_ gatewayID: String?, _ direction: String) -> String {
        let gw = gatewayID?.isEmpty == false ? gatewayID! : "default"
        return "denoise.\(direction).\(gw)"
    }

    /// OFF until the user explicitly turns a switch on.
    func uplink(for gatewayID: String?) -> Bool {
        guard let raw = defaults.object(forKey: Self.key(gatewayID, "uplink")) as? Bool else { return false }
        return raw
    }

    func downlink(for gatewayID: String?) -> Bool {
        guard let raw = defaults.object(forKey: Self.key(gatewayID, "downlink")) as? Bool else { return false }
        return raw
    }

    func setUplink(_ on: Bool, gatewayID: String?) {
        defaults.set(on, forKey: Self.key(gatewayID, "uplink"))
    }

    func setDownlink(_ on: Bool, gatewayID: String?) {
        defaults.set(on, forKey: Self.key(gatewayID, "downlink"))
    }

    func choice(for gatewayID: String?) -> DenoiseChoice {
        DenoiseChoice(uplink: uplink(for: gatewayID), downlink: downlink(for: gatewayID))
    }
}
