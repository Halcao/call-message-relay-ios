import SwiftUI

/// 骚扰拦截 (production): the per-enrollment-key gateway policy — master
/// call/SMS switches, blocked/trusted numbers, imported lists and block
/// history. Same-key devices share one policy; a different key on the same
/// physical line is isolated. On an older gateway the same screen falls back
/// to the on-device rules and says so honestly. Advanced keyword/prefix/regex
/// rules and testing live on the secondary 高级规则 screen.
struct SpamRulesView: View {
    @EnvironmentObject private var model: AppModel
    @ObservedObject private var store: SpamFilterStore
    @State private var newBlock = ""
    @State private var newTrust = ""
    @State private var blockKind: EntryKind = .exact
    @State private var importSheet: ImportTarget?
    @State private var note: String?
    @State private var showImportToGateway = false
    @State private var importingToGateway = false

    enum ImportTarget: String, Identifiable {
        case paste, url, file
        var id: String { rawValue }
    }

    enum EntryKind: String, CaseIterable {
        case exact, prefix
        var title: String { self == .exact ? "完整号码" : "号码前缀" }
        var matchKind: String { self == .exact ? "exact" : "prefix" }
    }

    init(store: SpamFilterStore) {
        _store = ObservedObject(initialValue: store)
    }

    private var harassment: HarassmentStore { model.harassment }
    private var gatewayPolicy: Bool { harassment.support == .supported }

    var body: some View {
        Form {
            if harassment.support == .unavailable {
                Section {
                    Text("当前网关版本较旧，拦截只在本设备生效。")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }
            if let error = harassment.lastError {
                Section { Text(error).font(.footnote).foregroundStyle(.orange) }
            }

            masterToggles
            blockedSection
            trustedSection
            listsSection
            historySection

            Section {
                NavigationLink {
                    AdvancedSpamRulesView(store: store)
                } label: {
                    Label("高级规则与测试", systemImage: "slider.horizontal.3")
                }
            }
        }
        .navigationTitle("骚扰拦截")
        .navigationBarTitleDisplayMode(.inline)
        .task { await model.refreshHarassmentPolicy() }
        .refreshable { await model.refreshHarassmentPolicy() }
        .sheet(item: $importSheet) { target in
            ImportListView(target: target) { imported in
                note = imported
            }
            .environmentObject(model)
        }
        .confirmationDialog(
            "把本机已启用的规则和名单导入网关？",
            isPresented: $showImportToGateway,
            titleVisibility: .visible
        ) {
            Button("导入现有规则") { Task { await runGatewayImport() } }
            Button("取消", role: .cancel) {}
        } message: {
            Text("导入的名单默认只标记不拦截，不会覆盖网关已有规则。")
        }
    }

    // MARK: Master switches

    private var masterToggles: some View {
        Section {
            Toggle("拦截来电", isOn: Binding(
                get: { store.callBlockingEnabled },
                set: { value in
                    Task { await model.setHarassmentToggles(callBlocking: value, smsBlocking: store.smsBlockingEnabled) }
                }
            ))
            .accessibilityIdentifier("callBlockingToggle")
            Toggle("拦截短信", isOn: Binding(
                get: { store.smsBlockingEnabled },
                set: { value in
                    Task { await model.setHarassmentToggles(callBlocking: store.callBlockingEnabled, smsBlocking: value) }
                }
            ))
            .accessibilityIdentifier("smsBlockingToggle")
        } header: {
            Text("拦截开关")
        } footer: {
            Text("未知号码默认放行。")
                .font(.caption)
        }
    }

    // MARK: Blocked / trusted numbers

    private var blockedSection: some View {
        Section {
            if gatewayPolicy {
                ForEach(harassment.blockedEntries) { entry in
                    entryRow(entry)
                }
                .onDelete { offsets in
                    let entries = harassment.blockedEntries
                    Task {
                        for index in offsets { _ = await harassment.deleteEntry(api: model.api, id: entries[index].id) }
                    }
                }
            } else {
                ForEach(localBlockRules) { rule in
                    Text(rule.value).lineLimit(1)
                }
                .onDelete { offsets in
                    let rules = localBlockRules
                    for index in offsets { store.removeRule(rules[index]) }
                }
            }
            HStack {
                TextField(blockKind == .exact ? "完整号码" : "号码前缀", text: $newBlock)
                    .keyboardType(.phonePad)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                Picker("", selection: $blockKind) {
                    ForEach(EntryKind.allCases, id: \.self) { Text($0.title).tag($0) }
                }
                .labelsHidden()
                .pickerStyle(.menu)
                Button("添加") { addBlock() }
                    .disabled(newBlock.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
            .accessibilityIdentifier("addBlockedNumberRow")
        } header: {
            Text("拦截号码")
        } footer: {
            Text("被拦截的来电直接拒接，不会转入语音留言。")
                .font(.caption)
        }
    }

    private var trustedSection: some View {
        Section {
            if gatewayPolicy {
                ForEach(harassment.trustedEntries) { entry in
                    entryRow(entry)
                }
                .onDelete { offsets in
                    let entries = harassment.trustedEntries
                    Task {
                        for index in offsets { _ = await harassment.deleteEntry(api: model.api, id: entries[index].id) }
                    }
                }
            } else {
                ForEach(localTrustRules) { rule in
                    Text(rule.value).lineLimit(1)
                }
                .onDelete { offsets in
                    let rules = localTrustRules
                    for index in offsets { store.removeRule(rules[index]) }
                }
            }
            HStack {
                TextField("完整号码", text: $newTrust)
                    .keyboardType(.phonePad)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                Button("添加") { addTrust() }
                    .disabled(newTrust.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        } header: {
            Text("信任号码")
        } footer: {
            Text("信任号码始终放行，优先于拦截规则。")
                .font(.caption)
        }
    }

    private func entryRow(_ entry: HarassmentEntryRecord) -> some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text(entry.number).lineLimit(1)
                Text(matchKindLabel(entry.matchKind) + (entry.isLabelOnly ? " · 仅标记" : ""))
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            if entry.source == "import" {
                Text("导入").font(.caption2).foregroundStyle(.secondary)
            }
        }
    }

    // MARK: Lists

    private var listsSection: some View {
        Section {
            if gatewayPolicy {
                ForEach(harassment.lists) { list in
                    VStack(alignment: .leading, spacing: 2) {
                        Text(list.name)
                        Text("\(list.entryCount) 个号码 · \(list.defaultAction == "reject" ? "拦截" : "仅标记")")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
                .onDelete { offsets in
                    let lists = harassment.lists
                    Task {
                        for index in offsets { _ = await harassment.deleteList(api: model.api, id: lists[index].id) }
                    }
                }
            }
            NavigationLink {
                NumberListsView(store: store)
            } label: {
                Label("号码名单", systemImage: "list.bullet.rectangle")
            }
            Button { importSheet = .paste } label: { Label("粘贴号码导入", systemImage: "doc.on.clipboard") }
            Button { importSheet = .file } label: { Label("从文件导入", systemImage: "doc.text") }
            Button { importSheet = .url } label: { Label("从 HTTPS 链接更新", systemImage: "globe") }
            if gatewayPolicy {
                Button { showImportToGateway = true } label: {
                    Label(importingToGateway ? "正在导入…" : "导入现有规则到网关", systemImage: "arrow.up.doc")
                }
                .disabled(importingToGateway)
                .accessibilityIdentifier("importRulesToGateway")
            }
            if let note {
                Text(note).font(.caption).foregroundStyle(.secondary)
            }
        } header: {
            Text("导入的名单")
        } footer: {
            Text("导入只在你确认后进行；导入名单默认只标记不拦截。")
                .font(.caption)
        }
    }

    // MARK: History

    private var historySection: some View {
        Section {
            if gatewayPolicy {
                if harassment.history.isEmpty {
                    Text("暂无拦截记录。").font(.caption).foregroundStyle(.secondary)
                }
                ForEach(harassment.history.prefix(50)) { event in
                    historyRow(peer: event.peer, kind: event.kind, action: event.action,
                               date: Date(timeIntervalSince1970: Double(event.createdAt) / 1000))
                }
                if !harassment.history.isEmpty {
                    Button(role: .destructive) {
                        Task { _ = await harassment.clearHistory(api: model.api) }
                    } label: {
                        Label("清空拦截记录", systemImage: "trash")
                    }
                }
            } else {
                if store.screeningHistory.isEmpty {
                    Text("暂无拦截记录。").font(.caption).foregroundStyle(.secondary)
                }
                ForEach(store.screeningHistory.prefix(50)) { event in
                    historyRow(peer: event.peer, kind: event.kind, action: event.action,
                               date: event.date)
                }
                if !store.screeningHistory.isEmpty {
                    Button(role: .destructive) {
                        store.clearScreeningHistory()
                    } label: {
                        Label("清空拦截记录", systemImage: "trash")
                    }
                }
            }
        } header: {
            Text("拦截记录")
        }
    }

    private func historyRow(peer: String, kind: String, action: String, date: Date) -> some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text(peer).lineLimit(1)
                Text("\(kind == "call" ? "来电" : "短信") · \(action == "label" ? "仅标记" : "已拦截")")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Text(date.formatted(date: .abbreviated, time: .shortened))
                .font(.caption2).foregroundStyle(.secondary)
        }
    }

    // MARK: Actions

    private func addBlock() {
        let value = newBlock.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty else { return }
        if gatewayPolicy {
            Task { _ = await harassment.addEntry(
                api: model.api, number: value, matchKind: blockKind.matchKind, mode: "block", action: "reject") }
        } else if blockKind == .prefix {
            store.addRule(kind: .numberPrefix, value: value, label: "已拦截号码前缀")
        } else {
            store.addRule(kind: .senderExact, value: value, label: "已拦截号码")
        }
        newBlock = ""
    }

    private func addTrust() {
        let value = newTrust.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty else { return }
        if gatewayPolicy {
            Task { _ = await harassment.addEntry(
                api: model.api, number: value, matchKind: "exact", mode: "trust", action: nil) }
        } else {
            store.addRule(kind: .whitelistSender, value: value, label: "信任号码")
        }
        newTrust = ""
    }

    private func runGatewayImport() async {
        importingToGateway = true
        defer { importingToGateway = false }
        let result = await model.importLocalRulesToGateway()
        var text: String
        if result.aborted {
            note = "配对已变更，导入已中止。"
            return
        }
        if result.failed == 0 {
            text = result.imported == 0 ? "本机没有可导入的规则。" : "已导入 \(result.imported) 条规则。"
        } else {
            text = "已导入 \(result.imported) 条，\(result.failed) 条失败，请稍后重试。"
        }
        note = text
    }

    private func matchKindLabel(_ kind: String) -> String {
        switch kind {
        case "prefix": return "号码前缀"
        case "keyword": return "关键词"
        case "regex": return "正则表达式"
        default: return "完整号码"
        }
    }

    private var localBlockRules: [SpamRule] {
        store.rules.filter { $0.kind == .senderExact || $0.kind == .numberPrefix }
    }

    private var localTrustRules: [SpamRule] {
        store.rules.filter { $0.kind == .whitelistSender }
    }
}

/// Secondary 高级规则 screen: conservative presets, editable
/// keyword/prefix/regex rules, the contact whitelist and the rule tester.
/// Reached only from 骚扰拦截 so the main screen stays short.
struct AdvancedSpamRulesView: View {
    @EnvironmentObject private var model: AppModel
    @ObservedObject private var store: SpamFilterStore
    @State private var newRuleKind: SpamRule.Kind = .keyword
    @State private var newRuleValue = ""
    @State private var previewPeer = "555-0100"
    @State private var previewBody = ""
    @State private var previewResult: String?
    @State private var gatewayVerdict: HarassmentDecisionRecord?

    init(store: SpamFilterStore) {
        _store = ObservedObject(initialValue: store)
    }

    var body: some View {
        Form {
            Section {
                Toggle("通讯录中的号码视为可信", isOn: Binding(
                    get: { model.contactWhitelistEnabled },
                    set: { model.contactWhitelistEnabled = $0 }
                ))
            } footer: {
                Text("你手动加入的拦截号码仍然优先生效。")
                    .font(.caption)
            }

            Section("保守预设（全部默认关闭）") {
                ForEach(SpamPreset.allCases, id: \.self) { preset in
                    Toggle(isOn: Binding(
                        get: { store.enabledPresets.contains(preset) },
                        set: { on in
                            if on { store.enable(preset: preset) } else { store.disable(preset: preset) }
                        }
                    )) {
                        Text(preset.displayName)
                    }
                    .accessibilityIdentifier("preset-\(preset.rawValue)")
                }
            }

            Section("规则测试") {
                TextField("发件号码", text: $previewPeer)
                    .keyboardType(.phonePad)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                TextField("短信内容", text: $previewBody, axis: .vertical)
                Button("测试") { Task { await runTest() } }
                    .accessibilityIdentifier("spamPreviewButton")
                if let previewResult {
                    Text(previewResult)
                        .font(.footnote)
                        .foregroundStyle(previewResult.contains("拦截") ? .orange : .secondary)
                        .accessibilityIdentifier("spamPreviewResult")
                }
                if let gatewayVerdict {
                    Text(gatewayVerdict.allow ? "网关规则：放行" : "网关规则：拦截")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }

            ruleSections
        }
        .navigationTitle("高级规则与测试")
        .navigationBarTitleDisplayMode(.inline)
    }

    @ViewBuilder private var ruleSections: some View {
        addRuleSection
        ForEach(SpamRule.Kind.allCases, id: \.self) { kind in
            let rules = store.rules.filter { $0.kind == kind }
            if !rules.isEmpty {
                Section(kind.displayName) {
                    ForEach(rules) { rule in
                        RuleRow(rule: rule) { updated in
                            store.update(rule: updated)
                        }
                    }
                    .onDelete { offsets in
                        let ids = offsets.map { rules[$0].id }
                        for id in ids { if let rule = store.rules.first(where: { $0.id == id }) { store.removeRule(rule) } }
                    }
                }
            }
        }
    }

    private var addRuleSection: some View {
        Section("添加规则") {
            Picker("类型", selection: $newRuleKind) {
                ForEach(SpamRule.Kind.allCases, id: \.self) { Text($0.displayName).tag($0) }
            }
            TextField(valueFieldTitle, text: $newRuleValue)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            Button("添加") {
                store.addRule(kind: newRuleKind, value: newRuleValue)
                newRuleValue = ""
            }
            .disabled(newRuleValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            .accessibilityIdentifier("addRuleButton")
        }
    }

    private var valueFieldTitle: String {
        switch newRuleKind {
        case .senderExact, .whitelistSender: return "完整号码/短号（如 555-0100、1069…）"
        case .numberPrefix: return "号码前缀（数字）"
        case .keyword, .whitelistKeyword: return "关键词（不区分大小写）"
        case .regex: return "正则表达式"
        }
    }

    private func runTest() async {
        let localVerdict = store.policy().classifySMS(
            peer: previewPeer, body: previewBody,
            isKnownSender: store.isKnownSender(previewPeer)
                || (model.contactWhitelistEnabled && model.contacts.name(forPeer: previewPeer) != nil)
        )
        switch localVerdict {
        case .junk: previewResult = "拦截"
        case .allow: previewResult = "放行"
        case .unknown: previewResult = "未知发件人不拦截"
        }
        gatewayVerdict = await model.harassment.test(
            api: model.api, kind: "sms", number: previewPeer, body: previewBody)
    }
}

private struct RuleRow: View {
    let rule: SpamRule
    let onChange: (SpamRule) -> Void

    var body: some View {
        HStack {
            TextField("规则", text: Binding(
                get: { rule.value },
                set: { var copy = rule; copy.value = $0; onChange(copy) }
            ))
            .textInputAutocapitalization(.never).autocorrectionDisabled()
            Toggle("", isOn: Binding(
                get: { rule.enabled },
                set: { var copy = rule; copy.enabled = $0; onChange(copy) }
            ))
            .labelsHidden()
        }
    }
}
