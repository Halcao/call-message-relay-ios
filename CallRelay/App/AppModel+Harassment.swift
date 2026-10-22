import Foundation

/// AppModel surface for the 骚扰拦截 feature and the recording auto
/// preference. The gateway policy is key-scoped and shared by same-key
/// devices; local rules remain the fallback for old gateways and never
/// auto-upload.
extension AppModel {
    // MARK: Gateway policy

    /// Loads the key policy (feature probe + settings/lists/history). Safe to
    /// call repeatedly; the result is fenced to the current pairing session.
    func refreshHarassmentPolicy() async {
        let gen = sessionGeneration
        let api = self.api
        await harassment.refresh(api: api)
        guard gen == sessionGeneration else { return }
        if harassment.support == .supported {
            // Keep the LOCAL fallback layer consistent with the shared
            // key policy, and mirror later local edits back to the gateway.
            spamFilter.applyGatewayToggles(
                callBlocking: harassment.callBlocking, smsBlocking: harassment.smsBlocking)
            // The contact-whitelist switch is shared per key too; contacts
            // themselves are only ever uploaded by the explicit contacts
            // sync action, never by this feature.
            defaults.set(harassment.contactWhitelist, forKey: DefaultsKey.contactWhitelist)
            inbox?.reevaluateAll()
            // The callback belongs to the session that INSTALLED it: capture
            // that generation and API now. A retained/queued old callback can
            // therefore never read the new pairing's api/support and write
            // stale preferences to the new key.
            let installedGen = gen
            let installedAPI = api
            spamFilter.onTogglesChanged = { [weak self] call, sms in
                Task { @MainActor in
                    guard let self, installedGen == self.sessionGeneration,
                          self.harassment.support == .supported else { return }
                    _ = await self.harassment.setToggles(
                        api: installedAPI, call: call, sms: sms)
                    guard installedGen == self.sessionGeneration else { return }
                }
            }
            // Per-key folders may be available now; refresh the SMS view so
            // the junk folder switches to gateway truth.
            await inbox?.refreshThreads()
        }
    }

    /// Mirror an edited contact-whitelist switch to the key-scoped gateway
    /// policy (no-op on old gateways, where the switch stays local).
    /// The caller captures the session/API at the moment the USER flipped the
    /// switch, so a delayed task can never apply an old-session preference to
    /// a newly paired key.
    func pushContactWhitelistToGateway(_ enabled: Bool, gen: UInt64, api: GatewayAPI?) async {
        guard harassment.support == .supported, gen == sessionGeneration else { return }
        _ = await harassment.setContactWhitelist(api: api, enabled)
        guard gen == sessionGeneration else { return }
    }

    func refreshRecordingConfig() async {
        let gen = sessionGeneration
        let api = self.api
        await recordingConfig.refresh(api: api)
        guard gen == sessionGeneration else { return }
    }

    func setHarassmentToggles(callBlocking: Bool, smsBlocking: Bool) async {
        let gen = sessionGeneration
        let api = self.api
        if harassment.support == .supported {
            let ok = await harassment.setToggles(api: api, call: callBlocking, sms: smsBlocking)
            // A pairing switch mid-flight discards the old result entirely:
            // no local mirror, no notice, no new-session mutation.
            guard gen == sessionGeneration else { return }
            if ok, harassment.lastError == nil,
               harassment.callBlocking == callBlocking, harassment.smsBlocking == smsBlocking {
                spamFilter.applyGatewayToggles(callBlocking: callBlocking, smsBlocking: smsBlocking)
            }
        } else {
            // Old gateway (or not paired): the master switches stay local.
            guard gen == sessionGeneration else { return }
            spamFilter.setCallBlocking(callBlocking, notifyGateway: false)
            spamFilter.setSMSBlocking(smsBlocking, notifyGateway: false)
        }
    }

    /// Context-menu block. Contextual intent is explicit: choosing
    /// 拦截此号码 must actually block the ORIGINATING channel, so this also
    /// enables that channel's master switch (leaving the other channel's
    /// preference untouched). On a capable gateway the key-scoped rule is
    /// written; otherwise the local rule applies. Returns false (with a
    /// visible notice) when the write fails — a revoked key or a version
    /// conflict is never reported as success.
    @discardableResult
    func blockHarassmentNumber(_ rawPeer: String, matchKind: String = "exact", channel: String = "call") async -> Bool {
        let number = rawPeer.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !number.isEmpty else { return false }
        let gen = sessionGeneration
        let api = self.api
        let isCall = channel != "sms"
        if harassment.support == .supported {
            let togglesOK = await harassment.setToggles(
                api: api,
                call: isCall ? true : harassment.callBlocking,
                sms: isCall ? harassment.smsBlocking : true)
            // Fenced: a changed pairing discards the old result silently (the
            // new session's state was already reset).
            guard gen == sessionGeneration else { return false }
            guard togglesOK else {
                harassment.setActionNotice(harassment.lastError ?? "无法启用拦截开关，请稍后重试。")
                return false
            }
            spamFilter.applyGatewayToggles(
                callBlocking: harassment.callBlocking, smsBlocking: harassment.smsBlocking)
            // replaceOpposite: an existing explicit exact trust for the same
            // (canonical) number is removed in the same write, so the block
            // is real instead of being silently overridden.
            let result = await harassment.addEntry(
                api: api, number: number, matchKind: matchKind, mode: "block",
                action: "reject", replaceOpposite: true)
            guard gen == sessionGeneration else { return false }
            guard let result else {
                harassment.setActionNotice(harassment.lastError ?? "拦截失败，请稍后重试。")
                return false
            }
            guard result.isEffective else {
                harassment.setActionNotice(result.note ?? "已有更宽的信任规则，该号码仍会放行。")
                return false
            }
            harassment.clearActionNotice()
            return true
        }
        // Local fallback: same contextual semantics, no gateway involved.
        if isCall {
            spamFilter.setCallBlocking(true, notifyGateway: false)
        } else {
            spamFilter.setSMSBlocking(true, notifyGateway: false)
        }
        if matchKind == "prefix" {
            spamFilter.addRule(kind: .numberPrefix, value: number, label: "已拦截号码前缀")
        } else {
            // Replace an explicit exact trust for this canonical number and
            // drop any known-sender mark: trusting, then blocking, must block.
            _ = spamFilter.removeRules(matching: number, kinds: [.whitelistSender])
            _ = spamFilter.removeKnownSenders(matching: number)
            spamFilter.addRule(kind: .senderExact, value: number, label: "已拦截号码")
            let hits = spamFilter.callListHits(for: number)
            if case .reject = spamFilter.policy().screenCall(peer: number, listHits: hits) {
                return true
            }
            harassment.setActionNotice("本机仍有更宽的信任规则，该号码仍会放行。")
            return false
        }
        return true
    }

    /// Context-menu trust. Replaces an explicit exact block for the same
    /// canonical number (same contextual replacement rule as blocking).
    @discardableResult
    func trustHarassmentNumber(_ rawPeer: String) async -> Bool {
        let number = rawPeer.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !number.isEmpty else { return false }
        let gen = sessionGeneration
        let api = self.api
        if harassment.support == .supported {
            let result = await harassment.addEntry(
                api: api, number: number, matchKind: "exact", mode: "trust",
                action: nil, replaceOpposite: true)
            guard gen == sessionGeneration else { return false }
            guard result != nil else {
                harassment.setActionNotice(harassment.lastError ?? "信任失败，请稍后重试。")
                return false
            }
            harassment.clearActionNotice()
            return true
        }
        _ = spamFilter.removeRules(matching: number, kinds: [.senderExact])
        spamFilter.markSenderKnown(number)
        spamFilter.addRule(kind: .whitelistSender, value: number, label: "信任号码")
        return true
    }

    /// Context-menu unblock: remove the matching key-scoped block entries and
    /// add a trust entry (with opposite-mode replacement) so no explicit
    /// exact block keeps silencing it.
    func unblockHarassmentNumber(_ rawPeer: String) async {
        let number = rawPeer.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !number.isEmpty else { return }
        let gen = sessionGeneration
        let api = self.api
        let keys = Set(PhoneNormalizer.canonicalKeys(number))
        if harassment.support == .supported {
            let matches = harassment.blockedEntries.filter { entry in
                !keys.isDisjoint(with: PhoneNormalizer.canonicalKeys(entry.number))
            }
            for entry in matches {
                guard gen == sessionGeneration else { return }
                _ = await harassment.deleteEntry(api: api, id: entry.id)
                guard gen == sessionGeneration else { return }
            }
            guard gen == sessionGeneration else { return }
            _ = await harassment.addEntry(
                api: api, number: number, matchKind: "exact", mode: "trust",
                action: nil, replaceOpposite: true)
        } else {
            _ = spamFilter.removeRules(matching: number, kinds: [.senderExact])
            spamFilter.markSenderKnown(number)
        }
    }

    /// True when this peer has an explicit local/gateway block rule, so the
    /// context menu can show 解除拦截 instead of 拦截此号码.
    func isHarassmentBlocked(_ rawPeer: String) -> Bool {
        let keys = Set(PhoneNormalizer.canonicalKeys(rawPeer))
        guard !keys.isEmpty else { return false }
        if harassment.support == .supported {
            return harassment.blockedEntries.contains { entry in
                !keys.isDisjoint(with: PhoneNormalizer.canonicalKeys(entry.number))
            }
        }
        return spamFilter.rules.contains { rule in
            rule.enabled && (rule.kind == .senderExact || rule.kind == .numberPrefix)
                && !keys.isDisjoint(with: PhoneNormalizer.canonicalKeys(rule.value))
        }
    }

    // MARK: Explicit one-time import

    /// The ONE explicit "导入现有规则" action. Never automatic: the owner taps
    /// it, the app uploads the LOCAL enabled rules, enabled conservative
    /// presets, marked-known trusted senders and non-off imported lists
    /// (imported entries default to mark-only label). Unsupported/local-only
    /// items are reported as retained instead of being claimed as migrated.
    /// The gateway rules are appended, never overwritten.
    @discardableResult
    func importLocalRulesToGateway() async -> HarassmentImportOutcome {
        let gen = sessionGeneration
        let api = self.api
        guard harassment.support == .supported else {
            return HarassmentImportOutcome(imported: 0, failed: 0, retainedLocal: ["当前网关不支持规则导入（保留本机）"])
        }
        let payloads = spamFilter.gatewayImportPayload().prefix(500)
        var imported = 0
        var failed = 0
        for payload in payloads {
            // Every iteration is fenced: an unpair/re-pair mid-import must
            // never send the remaining OLD payloads to the NEW key.
            guard gen == sessionGeneration else {
                return HarassmentImportOutcome(
                    imported: imported, failed: failed, aborted: true,
                    retainedLocal: ["配对已变更，导入已中止（保留本机）"])
            }
            // Bulk imports never replace opposite-mode rules: the configured
            // precedence (explicit trust > explicit block > ...) is preserved.
            let result = await harassment.addEntry(
                api: api, number: payload.number, matchKind: payload.matchKind,
                mode: payload.mode, action: payload.action, replaceOpposite: false)
            guard gen == sessionGeneration else {
                return HarassmentImportOutcome(
                    imported: imported, failed: failed, aborted: true,
                    retainedLocal: ["配对已变更，导入已中止（保留本机）"])
            }
            if result != nil { imported += 1 } else { failed += 1 }
        }
        var retained = spamFilter.gatewayImportRetainedLocal()
        let knownCount = spamFilter.knownSenders.count
        if knownCount == 0 && payloads.count == 0 {
            retained.append("没有可迁移的本机规则")
        }
        return HarassmentImportOutcome(imported: imported, failed: failed, retainedLocal: retained)
    }

    /// Recording auto preference (device half; default OFF). The gateway
    /// operator mode must also be on before anything records automatically.
    @discardableResult
    func setDeviceAutoRecord(_ enabled: Bool) async -> Bool {
        let gen = sessionGeneration
        let api = self.api
        let ok = await recordingConfig.setDeviceAutoRecord(api: api, enabled)
        guard gen == sessionGeneration else { return false }
        return ok
    }
}

/// Outcome of the explicit rule import, including what stayed local and an
/// honest aborted flag when a pairing change stopped the loop.
struct HarassmentImportOutcome: Equatable {
    let imported: Int
    let failed: Int
    var aborted: Bool = false
    let retainedLocal: [String]
}
