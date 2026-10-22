import Foundation

/// Conversation tap routed from a standard APNs SMS alert (push-type=alert,
/// category `CALLRELAY_SMS`). The gateway's envelope carries bounded
/// presentation fields (`peer`/`preview`, truncated server-side with an
/// ellipsis) AND complete routing metadata (`threadKey`, `gatewayId`,
/// `lineId`, `messageId`) in the top-level custom fields.
///
/// Privacy/coherence rules (root addendum):
/// * ONLY the routing metadata is parsed here. `peer`/`preview` are bounded
///   presentation data — this type deliberately does not even expose them,
///   so a truncated preview can never be mistaken for, persisted as, or
///   replace a full SMS record; the full body comes from the scoped cache or
///   a scoped fetch after the jump.
/// * `gatewayId` MUST match the stored pairing before any conversation is
///   opened; a foreign or missing identity never routes a jump.
/// * A `line_alert` service payload is never an SMS route (service alerts
///   keep their exclusive Settings routing).
struct SMSAlertRoute: Equatable, Sendable {
    static let alertCategory = "CALLRELAY_SMS"

    /// Gateway message id ("<lineId>:<workerMessageId>" global form).
    let messageId: String
    /// COMPLETE conversation key — the gateway guarantees routing metadata
    /// is never truncated (an oversized payload is rejected, not clipped).
    let threadKey: String
    let gatewayId: String?
    let lineId: String?

    /// Parses an APNs `userInfo` dictionary. Returns nil for anything that
    /// is not an SMS alert carrying complete routing metadata, so unrelated
    /// or partial payloads simply fall through.
    static func parse(userInfo: [AnyHashable: Any]) -> SMSAlertRoute? {
        if (userInfo["kind"] as? String) == ServiceAlertPayload.kindLineAlert { return nil }
        guard let aps = userInfo["aps"] as? [String: Any], aps["alert"] != nil else {
            return nil
        }
        if let category = aps["category"] as? String, category != alertCategory { return nil }
        guard let messageId = userInfo["messageId"] as? String,
              !messageId.isEmpty,
              let threadKey = userInfo["threadKey"] as? String,
              !threadKey.isEmpty else {
            return nil
        }
        return SMSAlertRoute(
            messageId: messageId,
            threadKey: threadKey,
            gatewayId: userInfo["gatewayId"] as? String,
            lineId: userInfo["lineId"] as? String
        )
    }

    /// True only when the payload names THIS pairing's gateway. The tap
    /// must never open a conversation against a gateway the user is not
    /// bound to.
    func belongs(toGatewayID boundGatewayID: String?) -> Bool {
        guard let gatewayId, let boundGatewayID else { return false }
        return !gatewayId.isEmpty && gatewayId == boundGatewayID
    }
}
