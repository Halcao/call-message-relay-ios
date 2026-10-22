import SwiftUI

/// Compact, native route control shown near the in-call status. Shows the
/// route ACTUALLY carrying audio (short label + fresh RTT), a subtle progress
/// during a safe live handover, and a native menu with checkmarks on the
/// selected Auto/Direct/Relay mode. Failed forced selections surface a concise
/// actionable notice (with a one-tap switch to auto) and never hang up.
///
/// Native controls only: system Menu/Picker, semantic colors, minimum 44 pt
/// targets, Dynamic Type, dark mode and iPad safe. No dashboard, no RTT
/// clutter on the keypad/home.
struct InCallRouteMenu: View {
    @EnvironmentObject private var model: AppModel

    private var state: CallRouteState? { model.routeState }
    private var currentMode: MediaRouteMode { state?.mode ?? model.preferredRouteMode }

    var body: some View {
        VStack(spacing: 6) {
            Menu {
                ForEach(MediaRouteMode.allCases) { mode in
                    Button {
                        model.setPreferredRouteMode(mode)
                    } label: {
                        Label(mode.title, systemImage: currentMode == mode
                              ? "checkmark"
                              : (mode == .auto ? "point.3.connected.trianglepath.dotted"
                                 : mode == .direct ? "antenna.radiowaves.left.and.right" : "globe"))
                    }
                }
                // Honest measured per-path TRANSPORT round trips (WS ping /
                // direct echo), explicitly distinct from end-to-end voice
                // latency; never a fabricated or static value — unmeasured or
                // expired says so, and the inactive path keeps its own label.
                Section {
                    HStack {
                        Label(MediaRouteKind.direct.shortLabel, systemImage: "antenna.radiowaves.left.and.right")
                        Spacer()
                        Text(model.routeDiagnostics.direct.label())
                            .foregroundStyle(.secondary)
                    }
                    HStack {
                        Label(MediaRouteKind.relay.shortLabel, systemImage: "globe")
                        Spacer()
                        Text(model.routeDiagnostics.relay.label())
                            .foregroundStyle(.secondary)
                    }
                }
                // 2026-10-06 routing policy: explicit direct/relay selections
                // and one explicit auto evaluation are LIVE actions. The
                // next-call note is shown only for a genuinely deferred
                // preference (pendingModeChange), never after a live switch.
                if state?.pendingModeChange == true {
                    Section {
                        Text(String(localized: "更改于下次通话生效。"))
                    }
                }
            } label: {
                label
            }
            .menuOrder(.fixed)
            .disabled(disabled)
            .accessibilityLabel(accessibilityLabel)
            .accessibilityValue(accessibilityValue)

            // One non-blocking notice bar, never a modal alert on top of a
            // live call: the bar already carries the same recovery action
            // (switch to auto) and an explicit dismiss control.
            if let notice = model.routeNotice {
                noticeBar(notice)
            }
        }
    }

    /// The menu stays ENABLED while a switch is in progress: picking another
    /// mode cancels the in-flight (cancellable) attempt and applies the new
    /// selection immediately. Only a merged conference locks routing.
    private var disabled: Bool {
        state?.conferenceLocked ?? false
    }

    @ViewBuilder
    private var label: some View {
        HStack(spacing: 6) {
            if state?.switching == true {
                ProgressView()
                    .controlSize(.mini)
            } else {
                Image(systemName: state?.active == .direct
                      ? "antenna.radiowaves.left.and.right" : "globe")
                    .font(.caption.weight(.semibold))
            }
            Text(shortStatus)
                .font(.caption.weight(.semibold))
                .lineLimit(1)
                .minimumScaleFactor(0.8)
            Image(systemName: "chevron.up.chevron.down")
                .font(.caption2)
                .accessibilityHidden(true)
        }
        .padding(.horizontal, 12)
        .frame(minHeight: 44)
        .background(.thinMaterial, in: Capsule())
        .overlay(Capsule().stroke(Color(.separator), lineWidth: 0.5))
        .foregroundStyle(tint)
        .contentShape(Capsule())
    }

    private func noticeBar(_ notice: String) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill")
                .font(.caption)
                .foregroundStyle(.orange)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 4) {
                Text(notice)
                    .font(.caption)
                    .multilineTextAlignment(.leading)
                if model.routeNoticeOffersAuto {
                    Button {
                        model.switchRouteToAuto()
                    } label: {
                        Text("改用自动模式")
                            .font(.caption.weight(.semibold))
                    }
                    .buttonStyle(.borderless)
                    .frame(minHeight: 32)
                }
            }
            Spacer(minLength: 0)
            Button {
                model.dismissRouteNotice()
            } label: {
                Image(systemName: "xmark.circle.fill")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
            .frame(minWidth: 44, minHeight: 44)
            .accessibilityLabel(String(localized: "关闭提示"))
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .frame(maxWidth: 420)
        .background(Color.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 12))
    }

    private var tint: Color {
        if state?.switching == true { return .secondary }
        switch state?.active {
        case .direct: return .green
        case .relay: return .accentColor
        default: return .secondary
        }
    }

    private var shortStatus: String {
        guard let state else {
            return MediaRouteMode.auto.shortLabel
        }
        if state.conferenceLocked {
            return String(localized: "会议线路")
        }
        if state.switching { return String(localized: "切换中…") }
        // The ACTUAL transport, reconciled with the gateway; show its short
        // label and a fresh RTT when available (no dashboard clutter).
        switch state.active {
        case .direct, .relay:
            return state.statusLine
        case .none:
            return state.probing ? String(localized: "正在测试直连…") : state.mode.shortLabel
        }
    }

    private var accessibilityLabel: String {
        String(localized: "音频线路")
    }

    private var accessibilityValue: String {
        guard let state else { return "" }
        var value = state.active == .direct
            ? String(localized: "直连") : String(localized: "中继")
        if state.switching { value = String(localized: "切换中") }
        return value
    }
}

/// Screenshot-only fixture (launch-argument gated): all 0..4 signal-bar
/// states in one deterministic surface for the visual review.
struct SignalBarsPreviewFixture: View {
    private let states: [Int?] = [0, 1, 2, 3, 4, nil]

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("蜂窝信号 0–4 格")
                .font(.headline)
            ForEach(Array(states.enumerated()), id: \.offset) { _, bars in
                HStack(spacing: 14) {
                    Text(bars.map(String.init) ?? "未知")
                        .font(.subheadline.monospacedDigit())
                        .frame(width: 36, alignment: .trailing)
                    CellularSignalBars(bars: bars)
                    Text(CellularSignalBars(bars: bars).accessibilityText)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                .frame(minHeight: 44)
            }
            Spacer()
        }
        .padding(24)
        .frame(maxWidth: 480, alignment: .leading)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

/// One native Settings row: the persisted default route mode for this
/// gateway, using the system navigation-link picker (checkmark list,
/// Dynamic Type, dark mode, 44pt targets).
struct RouteModeSettingRow: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        NavigationLink {
            RouteModePickerView()
        } label: {
            Label {
                HStack {
                    Text(String(localized: "音频线路"))
                    Spacer()
                    // Actual route + freshest measured transport RTT, or the
                    // preference plus the measured candidate when idle.
                    Text(model.routeDiagnostics.connectionSummary(preferred: model.preferredRouteMode))
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.trailing)
                }
            } icon: {
                Image(systemName: "point.3.connected.trianglepath.dotted")
            }
        }
    }
}

struct RouteModePickerView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        Form {
            Section {
                ForEach(MediaRouteMode.allCases) { mode in
                    Button {
                        model.setPreferredRouteMode(mode)
                        dismiss()
                    } label: {
                        HStack {
                            Label(mode.title, systemImage: mode.sfSymbol)
                                .foregroundStyle(.primary)
                            Spacer()
                            if model.preferredRouteMode == mode {
                                Image(systemName: "checkmark")
                                    .foregroundStyle(Color.accentColor)
                            }
                        }
                        .frame(minHeight: 44)
                    }
                    .buttonStyle(.plain)
                }
            }
            // Build-61: QUIC-capable gateways get a direct-transport
            // sub-chooser (自动 / QUIC 直连 / 现有直连). Hidden entirely on
            // older gateways so nothing new appears where QUIC is not
            // advertised. Forced picks persist per gateway; a live call keeps
            // its pinned route until the coordinator enforces the choice.
            if model.directTransportChoicesAvailable {
                Section {
                    ForEach(DirectTransportChoice.allCases) { choice in
                        Button {
                            model.setDirectTransportChoice(choice)
                        } label: {
                            HStack {
                                Text(choice.title)
                                    .foregroundStyle(.primary)
                                Spacer()
                                if model.preferredDirectTransport == choice {
                                    Image(systemName: "checkmark")
                                        .foregroundStyle(Color.accentColor)
                                }
                            }
                            .frame(minHeight: 44)
                        }
                        .buttonStyle(.plain)
                    }
                } header: {
                    Text(String(localized: "直连方式"))
                }
                .accessibilityIdentifier("directTransportPicker")
            }
            // Chosen-path status: one row per candidate with its freshest
            // measured stats (RTT / 抖动 / 丢包). Queue/backlog numbers carry
            // the explicit "（估计）" suffix — they are estimates, never
            // presented as measured. Missing ⇒ 未测量; not usable ⇒ 不可用.
            if model.directTransportChoicesAvailable {
                Section {
                    ForEach(RouteCandidateKind.allCases, id: \.self) { kind in
                        HStack {
                            Text(kind.title)
                                .foregroundStyle(.primary)
                            Spacer()
                            Text(routeCandidateStatusText(kind))
                                .foregroundStyle(.secondary)
                                .multilineTextAlignment(.trailing)
                            if model.chosenRouteKind == kind {
                                Image(systemName: "checkmark.circle.fill")
                                    .foregroundStyle(.green)
                                    .accessibilityHidden(true)
                            }
                        }
                        .frame(minHeight: 44)
                        .accessibilityIdentifier(kind == .quicDirect
                                                 ? "routeQuicStatus"
                                                 : "routeCandidateStatus_\(kind.rawValue)")
                    }
                } header: {
                    Text(String(localized: "当前路径"))
                }
                .accessibilityIdentifier("chosenRoutePath")
            }
            // Measured per-path transport status (direct candidate echo RTT /
            // WSS relay ping RTT) reusing the existing measurements. Values
            // are never zeroed or faked; stale shows its age.
            Section {
                HStack {
                    Label(MediaRouteKind.direct.shortLabel, systemImage: "antenna.radiowaves.left.and.right")
                    Spacer()
                    Text(model.routeDiagnostics.direct.label())
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.trailing)
                }
                .accessibilityIdentifier("routeDirectStatus")
                HStack {
                    Label(MediaRouteKind.relay.shortLabel, systemImage: "globe")
                    Spacer()
                    Text(model.routeDiagnostics.relay.label())
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.trailing)
                }
                .accessibilityIdentifier("routeRelayStatus")
                // Build-50 hot standby: truthful freshness of the warm spare
                // (connected/ready or connecting/unavailable). Never a fake
                // zero; RTT shows only when measured.
                if let standby = model.hotStandbyState, let kind = standby.standby {
                    HStack {
                        Label(kind == .direct ? MediaRouteKind.direct.shortLabel : MediaRouteKind.relay.shortLabel,
                              systemImage: "cable.connector")
                        Spacer()
                        Text(standby.promotionInFlight
                             ? String(localized: "切换中")
                             : standby.standbyFresh
                             ? String(localized: "备用就绪")
                             : String(localized: "备用不可用"))
                            .foregroundStyle(.secondary)
                            .multilineTextAlignment(.trailing)
                    }
                    .accessibilityIdentifier("routeStandbyStatus")
                }
                if !model.routeDiagnostics.inCall {
                    Button {
                        model.recheckRouteMeasurement()
                    } label: {
                        Label(String(localized: "重新检测"), systemImage: "arrow.clockwise")
                    }
                    .disabled(model.routeDiagnostics.direct.state == .probing)
                    .accessibilityIdentifier("routeRecheckButton")
                }
            } header: {
                Text(String(localized: "线路延迟"))
            }
            // Live while the call runs, explicitly HISTORICAL after it ends:
            // a frozen sample must never sit under a "实时更新" footer
            // (build-33 screenshot showed 1–2 minute old values with a
            // real-time claim).
            if model.routeDiagnostics.inCall || model.routeDiagnostics.hasHistoricalTelemetry {
                Section {
                    ForEach(model.routeDiagnostics.telemetryRows(), id: \.label) { row in
                        HStack {
                            Text(row.label)
                            Spacer()
                            Text(row.value)
                                .foregroundStyle(row.isCurrent ? Color.secondary : Color.secondary.opacity(0.6))
                                .multilineTextAlignment(.trailing)
                        }
                    }
                } header: {
                    Text(model.routeDiagnostics.telemetryHeading())
                } footer: {
                    Text(model.routeDiagnostics.telemetryFooter())
                }
            }
            if model.routeState?.pendingModeChange == true {
                Section {
                    Text(String(localized: "更改于下次通话生效。"))
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .navigationTitle(String(localized: "音频线路"))
        .navigationBarTitleDisplayMode(.inline)
        .onAppear {
            // Opening the detail is an explicit intent to see current
            // measurements: start a fresh bounded idle preflight (no mic, no
            // call, no parallel in-call probing — ignored while a call runs).
            if !model.routeDiagnostics.inCall {
                model.recheckRouteMeasurement()
            }
        }
    }

    /// Compact per-candidate value for the 当前路径 rows: fresh measured
    /// "42 ms · 抖动 3 ms · 丢包 0.0%", a queue/backlog ESTIMATE fragment
    /// with the explicit "（估计）" suffix, 不可用 when the path is not
    /// usable, 未测量 when no sample exists. Values older than the freshness
    /// TTL are not presented as current (same honesty contract as the
    /// existing route rows).
    private func routeCandidateStatusText(_ kind: RouteCandidateKind) -> String {
        guard let sample = model.routeCandidateStats[kind] else {
            return String(localized: "未测量")
        }
        let age = Date().timeIntervalSince(sample.measuredAt)
        guard sample.available, age <= 10 else {
            return String(localized: "不可用")
        }
        let rtt = Int(sample.rttMs.rounded())
        let jitter = Int(sample.jitterMs.rounded())
        let loss = String(format: "%.1f%%", max(0, sample.lossFraction) * 100)
        var text = String(localized: "\(rtt) ms · 抖动 \(jitter) ms · 丢包 \(loss)")
        if sample.queueCostMs > 0 {
            let queue = Int(sample.queueCostMs.rounded())
            text += " · " + String(localized: "缓冲 \(queue) ms（估计）")
        }
        return text
    }
}
