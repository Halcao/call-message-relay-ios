import SwiftUI

/// DenoiseSettingRow exposes the two independent, build-48 directional voice
/// denoise switches ("我的声音降噪" = the local mic / uplink; "对方声音降噪"
/// = the remote/modem downlink). Both default ON (light SpeexDSP policy) and
/// apply per gateway/device; the choice is stored locally and synced to the
/// gateway's per-device preferences so the daemon applies it, including as a
/// live frame-boundary toggle for calls already attached.
struct DenoiseSettingRow: View {
    @EnvironmentObject private var model: AppModel
    @State private var uplink = DenoisePreferenceStore.shared.uplink(for: nil)
    @State private var downlink = DenoisePreferenceStore.shared.downlink(for: nil)

    private var gatewayID: String? { model.currentIntentPrincipal == "demo" ? nil : model.currentIntentPrincipal }

    var body: some View {
        Section(String(localized: "通话降噪")) {
            Toggle(String(localized: "我的声音降噪"), isOn: Binding(
                get: { uplink },
                set: { uplink = $0; persist(uplink: $0, downlink: nil) }
            ))
            .accessibilityIdentifier("denoiseUplinkToggle")
            Toggle(String(localized: "对方声音降噪"), isOn: Binding(
                get: { downlink },
                set: { downlink = $0; persist(uplink: nil, downlink: $0) }
            ))
            .accessibilityIdentifier("denoiseDownlinkToggle")
        }
        .onAppear(perform: refresh)
    }

    private func refresh() {
        let id = gatewayID
        uplink = DenoisePreferenceStore.shared.uplink(for: id)
        downlink = DenoisePreferenceStore.shared.downlink(for: id)
    }

    private func persist(uplink up: Bool?, downlink down: Bool?) {
        let id = gatewayID
        if let up { DenoisePreferenceStore.shared.setUplink(up, gatewayID: id) }
        if let down { DenoisePreferenceStore.shared.setDownlink(down, gatewayID: id) }
        // Best-effort sync to the gateway's per-device preference; the local
        // value and the next media attach params remain authoritative even
        // if this PATCH is offline.
        Task { await model.updateDenoisePreference(uplink: up, downlink: down) }
    }
}
