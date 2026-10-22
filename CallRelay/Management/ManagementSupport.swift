import Foundation
import SwiftUI

/// One implementation of the formatting vocabulary shared by every
/// gateway-administrator screen: times, durations, byte sizes and the state
/// vocabularies the gateway sends on the wire.
enum AdminFormat {
    /// Localized short date/time ("MM-dd HH:mm"); "—" for a missing stamp.
    static func time(_ millis: Int64?) -> String {
        guard let millis, millis > 0 else { return "—" }
        let formatter = DateFormatter()
        formatter.locale = .current
        formatter.dateFormat = "MM-dd HH:mm"
        return formatter.string(from: Date(unixMilliseconds: millis))
    }

    /// "m:ss" for a duration in milliseconds.
    static func duration(_ ms: Int64) -> String {
        let seconds = max(0, ms) / 1000
        return String(format: "%d:%02d", seconds / 60, seconds % 60)
    }

    /// "1.2 MB" / "512 KB" / "12 B".
    static func bytes(_ value: Int64) -> String {
        let bytes = max(0, value)
        if bytes >= 1_048_576 {
            return String(format: "%.1f MB", Double(bytes) / 1_048_576)
        }
        if bytes >= 1024 {
            return String(format: "%.0f KB", Double(bytes) / 1024)
        }
        return "\(bytes) B"
    }

    /// Call state vocabulary (see the gateway's call states). Unknown values
    /// are shown verbatim rather than guessed.
    static func stateLabel(_ state: String) -> String {
        switch state {
        case "idle": return "空闲"
        case "incoming_ringing": return "来电振铃"
        case "outgoing_dialing": return "正在外呼"
        case "connecting", "answering": return "接通中"
        case "active": return "通话中"
        case "held": return "保持中"
        case "ending": return "结束中"
        case "ended": return "已结束"
        case "failed": return "失败"
        case "recovering": return "恢复中"
        default:
            if state.hasPrefix("voicemail") { return "留言" }
            return state.isEmpty ? "未知" : state
        }
    }

    static func conferenceStateLabel(_ state: String) -> String {
        switch state {
        case "active": return "进行中"
        case "ended", "closed": return "已结束"
        case "starting", "ringing": return "接入中"
        default: return state.isEmpty ? "未知" : state
        }
    }

    static func rejectPolicyLabel(_ policy: String) -> String {
        switch policy {
        case "none": return "不拦截"
        case "reject_all": return "全部拒接"
        case "reject_blocked": return "拒接黑名单前缀"
        default: return policy.isEmpty ? "未知" : policy
        }
    }

    static func numberSourceLabel(_ source: String) -> String {
        switch source {
        case "sim": return "SIM 卡"
        case "manual": return "手动设置"
        case "empty": return "未读取"
        default: return source.isEmpty ? "未知" : source
        }
    }

    static func registrationLabel(_ registration: String) -> String {
        switch registration {
        case "registered": return "已注册"
        case "searching": return "搜索中"
        case "denied": return "注册被拒"
        default: return "未知"
        }
    }
}

/// Inline feedback for a console section: the SAME error/notice the root shows,
/// rendered inside the screen the user is actually on. Without it a failed
/// toggle on a child screen looks like nothing happened until they navigate
/// back (root review, build 68).
struct AdminFeedbackSection: View {
    @ObservedObject var center: AdminCenter

    var body: some View {
        if center.errorText != nil || center.noticeText != nil {
            Section {
                if let error = center.errorText {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .accessibilityIdentifier("managementSectionError")
                }
                if let notice = center.noticeText {
                    Text(notice)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }
}

extension View {
    /// Console-section gate: renders the section while an administrator session
    /// exists, and the login prompt IN PLACE otherwise. A 401 (or an explicit
    /// logout) from ANY child screen therefore leads straight back to logging
    /// in instead of leaving a screen whose actions silently do nothing.
    @ViewBuilder
    func adminSectionGate(center: AdminCenter) -> some View {
        if center.isSignedIn {
            self
        } else {
            ManagementLoginView(center: center)
        }
    }
}

/// Clears `password` when the screen goes away or a sign-in attempt ends, so
/// the secret never lingers in view state.
struct PasswordFieldCleanup: ViewModifier {
    @Binding var password: String
    var onDisappear: () -> Void

    func body(content: Content) -> some View {
        content.onDisappear {
            password = ""
            onDisappear()
        }
    }
}
