import XCTest

/// Batched visual review: deterministic, offline XCTest screenshots of the
/// Auto/Direct/Relay surfaces (Settings picker + in-call route menu) and the
/// 0..4 signal-bar states. Launch-argument fixtures only — no gateway, no
/// credentials, no physical audio claims.
///
/// The same test runs on iPhone (compact) and iPad (sidebar-adaptable)
/// destinations and captures light+dark with English labels, so the
/// coordinator can inspect exact pixels from the resulting xcresult.
final class VisualReviewUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUpWithError() throws {
        continueAfterFailure = false
        app = XCUIApplication()
    }

    override func tearDownWithError() throws {
        app = nil
        XCUIDevice.shared.appearance = .light
    }

    private func launch(_ args: [String]) {
        app.launchArguments = [
            "-callrelayUITestReset",
            "-AppleLanguages", "(en)",
            "-AppleLocale", "en_US"
        ] + args
        app.launch()
    }

    private func setAppearance(_ style: XCUIDevice.Appearance) {
        XCUIDevice.shared.appearance = style
        // Give the presented sheet the time to re-render under the new
        // interface style before the screenshot is taken.
        let deadline = Date().addingTimeInterval(1.2)
        while Date() < deadline {
            RunLoop.current.run(until: Date().addingTimeInterval(0.1))
        }
    }

    private func attach(_ name: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    private func captureBothAppearances(_ namePrefix: String) {
        setAppearance(.light)
        attach("\(namePrefix)-light")
        setAppearance(.dark)
        attach("\(namePrefix)-dark")
    }

    /// Average luminance of one screenshot, used to VERIFY the capture really
    /// rendered under the requested appearance instead of trusting the file
    /// name (build 54 shipped a "light" capture that was actually dark).
    private func averageLuminance(_ screenshot: XCUIScreenshot) -> Double {
        guard let image = screenshot.image.cgImage else { return -1 }
        var pixel = [UInt8](repeating: 0, count: 4)
        let space = CGColorSpaceCreateDeviceRGB()
        guard let context = CGContext(
            data: &pixel, width: 1, height: 1, bitsPerComponent: 8, bytesPerRow: 4,
            space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue
        ) else { return -1 }
        context.draw(image, in: CGRect(x: 0, y: 0, width: 1, height: 1))
        let r = Double(pixel[0]) / 255, g = Double(pixel[1]) / 255, b = Double(pixel[2]) / 255
        return 0.2126 * r + 0.7152 * g + 0.0722 * b
    }

    /// Capture light+dark and fail when the actual rendered trait does not
    /// match (light must be measurably brighter than dark).
    private func captureVerifiedAppearances(_ namePrefix: String) {
        setAppearance(.light)
        let light = averageLuminance(app.screenshot())
        attach("\(namePrefix)-light")
        setAppearance(.dark)
        let dark = averageLuminance(app.screenshot())
        attach("\(namePrefix)-dark")
        XCTAssertGreaterThan(light, dark + 0.05,
                             "\(namePrefix): light capture must be brighter than dark (light=\(light) dark=\(dark))")
    }

    private func button(containing label: String) -> XCUIElement {
        app.buttons.matching(NSPredicate(format: "label CONTAINS %@", label)).firstMatch
    }

    /// Any element (NavigationLink rows render as different types across OS
    /// versions) whose label contains the text.
    private func element(containing label: String) -> XCUIElement {
        app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS %@", label)).firstMatch
    }

    /// Settings → Audio route picker (one native screen with checkmarks).
    func testRouteSettingsPickerFixture() throws {
        launch(["-callrelayRouteSettingsPreview"])
        let settings = button(containing: "Settings")
        XCTAssertTrue(settings.waitForExistence(timeout: 8), "Settings destination must exist")
        settings.tap()
        let row = button(containing: "Audio route")
        XCTAssertTrue(row.waitForExistence(timeout: 8), "route picker row must render")
        attach("settings-route-row")

        row.tap()
        let pickerTitle = app.navigationBars.matching(
            NSPredicate(format: "identifier CONTAINS %@", "Audio route")).firstMatch
        XCTAssertTrue(pickerTitle.waitForExistence(timeout: 5), "route picker screen must open")
        captureBothAppearances("settings-route-picker")
    }

    /// Active demo call → compact route menu near the call status.
    func testInCallRouteMenuFixture() throws {
        launch(["-callrelayRoutePreview"])
        let menu = button(containing: "Audio route")
        XCTAssertTrue(menu.waitForExistence(timeout: 10), "in-call route menu must render")

        setAppearance(.light)
        attach("incall-route-menu-light")
        menu.tap()
        // The menu opens with the three modes and a checkmark on the selected.
        XCTAssertTrue(button(containing: "Direct").waitForExistence(timeout: 5),
                      "route menu must offer Direct")
        attach("incall-route-menu-open-light")
        app.tap() // dismiss the menu without changing the mode

        setAppearance(.dark)
        // Re-open under dark so both the sheet and the menu render dark.
        menu.tap()
        XCTAssertTrue(button(containing: "Relay").waitForExistence(timeout: 5))
        attach("incall-route-menu-open-dark")
        app.tap()
        setAppearance(.dark)
        attach("incall-route-menu-dark")
    }

    /// 0..4 signal-bar states in one deterministic strip.
    func testSignalBarsFixture() throws {
        launch(["-callrelaySignalBarsPreview"])
        XCTAssertTrue(app.staticTexts["蜂窝信号 0–4 格"].waitForExistence(timeout: 8),
                      "signal-bar fixture must render")
        captureBothAppearances("signal-bars-0to4")
    }

    // MARK: - Polish 52 everyday-copy surfaces (primary language)

    private func launchChinese(_ args: [String]) {
        app.launchArguments = [
            "-callrelayUITestReset",
            "-AppleLanguages", "(zh-Hans)",
            "-AppleLocale", "zh_CN"
        ] + args
        app.launch()
    }

    private func back(_ title: String) {
        app.navigationBars[title].buttons.element(boundBy: 0).tap()
    }

    /// The everyday screens whose copy was distilled in polish 52: Settings,
    /// spam rules and iCloud sync (paired synthetic lines, no network), plus
    /// the off-line route picker and in-call route menu fixtures. Light+dark
    /// on Settings and the route picker; single capture elsewhere.
    func testPolish52EverydaySurfaces() throws {
        launchChinese(["-callrelayPairedFixture"])
        XCTAssertTrue(app.selectSection("设置"), "Settings destination must exist")
        captureBothAppearances("polish52-settings-top")
        setAppearance(.light)

        let spam = element(containing: "骚扰拦截")
        for _ in 0..<6 where !spam.exists { app.swipeUp() }
        XCTAssertTrue(spam.waitForExistence(timeout: 5))
        spam.tap()
        XCTAssertTrue(app.navigationBars["骚扰拦截"].waitForExistence(timeout: 5))
        // Real light AND dark captures, verified by measured luminance (the
        // build-54 "light" shot was actually dark).
        captureVerifiedAppearances("build55-harassment-main")
        // The advanced screen is the secondary destination; capture it too.
        let advanced = element(containing: "高级规则与测试")
        for _ in 0..<8 where !advanced.exists { app.swipeUp() }
        XCTAssertTrue(advanced.waitForExistence(timeout: 5))
        advanced.tap()
        XCTAssertTrue(app.navigationBars["高级规则与测试"].waitForExistence(timeout: 5))
        captureVerifiedAppearances("build55-harassment-advanced")
        back("高级规则与测试")
        back("骚扰拦截")

        // Lower half of Settings: the iCloud entry and the gateway/service
        // sections whose footers were distilled. The iCloud DETAIL screen is
        // deliberately NOT entered: in the unsigned simulator build CloudKit
        // raises on CKContainer(identifier:) (no entitlement), which is a
        // fixture-only crash unrelated to this copy change.
        app.swipeUp()
        attach("polish52-settings-lower")

        // Route picker + in-call menu in the primary language (offline fixtures).
        app.terminate()
        launchChinese(["-callrelayRouteSettingsPreview"])
        XCTAssertTrue(app.selectSection("设置"), "Settings destination must exist")
        attach("polish52-settings-route-denoise")
        let route = button(containing: "音频线路")
        for _ in 0..<6 where !route.exists { app.swipeUp() }
        XCTAssertTrue(route.waitForExistence(timeout: 5))
        route.tap()
        XCTAssertTrue(app.navigationBars["音频线路"].waitForExistence(timeout: 5))
        captureBothAppearances("polish52-route-picker")

        app.terminate()
        launchChinese(["-callrelayRoutePreview"])
        let inCallRoute = button(containing: "音频线路")
        XCTAssertTrue(inCallRoute.waitForExistence(timeout: 10),
                      "in-call route menu must render")
        attach("polish52-incall-route-closed")
        inCallRoute.tap()
        XCTAssertTrue(button(containing: "直连").waitForExistence(timeout: 5),
                      "route menu must offer Direct")
        attach("polish52-incall-route-open")
    }

    /// One large Dynamic Type pass (accessibility XXXL) over Settings.
    func testPolish52SettingsLargeType() throws {
        launchChinese([
            "-callrelayPairedFixture",
            "-UIPreferredContentSizeCategoryName",
            "UICTContentSizeCategoryAccessibilityXXXL"
        ])
        XCTAssertTrue(app.selectSection("设置"), "Settings destination must exist")
        attach("polish52-settings-large-type")
    }

    // MARK: - Build 55 supported-gateway harassment surfaces (synthetic fixture)

    /// Synthetic harassment-CAPABLE gateway: proves the supported branch
    /// renders the key-shared toggles, gateway blocked/trusted entries, the
    /// imported list and the block history — plus the explicit
    /// 导入现有规则到网关 action. Fictional numbers only; no network.
    /// The older local-fallback captures remain as compatibility evidence.
    func testBuild55HarassmentGatewayFixture() throws {
        launchChinese(["-callrelayHarassmentGatewayPreview"])
        XCTAssertTrue(app.selectSection("设置"), "Settings destination must exist")
        let spam = element(containing: "骚扰拦截")
        for _ in 0..<6 where !spam.exists { app.swipeUp() }
        XCTAssertTrue(spam.waitForExistence(timeout: 8))
        spam.tap()
        XCTAssertTrue(app.navigationBars["骚扰拦截"].waitForExistence(timeout: 8))
        // Supported branch: key-shared toggles + gateway footer.
        XCTAssertTrue(app.switches["callBlockingToggle"].waitForExistence(timeout: 5),
                      "gateway-supported toggles must render")
        XCTAssertTrue(element(containing: "未知号码默认放行").waitForExistence(timeout: 5),
                      "supported footer must render")
        captureVerifiedAppearances("build55-harassment-gateway")

        // Scroll to the gateway entries / imported list / history area and
        // capture it too (the explicit import action must be visible).
        let importAction = element(containing: "导入现有规则到网关")
        for _ in 0..<10 where !importAction.exists { app.swipeUp() }
        XCTAssertTrue(importAction.waitForExistence(timeout: 8),
                      "explicit import action must be visible on a capable gateway")
        XCTAssertTrue(element(containing: "旧手机导入名单").exists,
                      "gateway imported list must render")
        XCTAssertTrue(element(containing: "已拦截").exists, "block history must render")
        captureVerifiedAppearances("build55-harassment-gateway-import")
    }

    // MARK: - Build 53 manual recording surfaces (offline synthetic fixture)

    /// In-call record indicator/control plus the RecordingsView list, using
    /// only fictional 张三/李四 fixture data. No gateway, credentials or audio.
    func testRecordingSurfacesFixture() throws {
        launchChinese(["-callrelayRecordingPreview"])
        let indicator = app.descendants(matching: .any)["call.recordingIndicator"].firstMatch
        XCTAssertTrue(indicator.waitForExistence(timeout: 12),
                      "in-call recording indicator must render")
        XCTAssertTrue(button(containing: "停止录音").waitForExistence(timeout: 5),
                      "record control must offer stop while recording")
        setAppearance(.light)
        attach("recording-incall-light")
        setAppearance(.dark)
        attach("recording-incall-dark")

        // Recordings list: Recents toolbar entry → list with fixture rows.
        // A dedicated list-only launch keeps the tab bar visible (the in-call
        // cover hides it in the first fixture).
        app.terminate()
        launchChinese(["-callrelayRecordingListPreview"])
        XCTAssertTrue(app.selectSection("最近通话"), "Recents destination must exist")
        let entry = app.buttons["recentsRecordings"]
        XCTAssertTrue(entry.waitForExistence(timeout: 8), "recordings toolbar entry must exist")
        entry.tap()
        XCTAssertTrue(app.navigationBars["通话录音"].waitForExistence(timeout: 8),
                      "recordings list must open")
        // Rows are combined accessibility elements: match on the combined
        // label instead of child static texts.
        let zhangsan = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS %@", "张三")).firstMatch
        XCTAssertTrue(zhangsan.waitForExistence(timeout: 5), "synthetic 张三 row must render")
        let lisi = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS %@", "李四")).firstMatch
        XCTAssertTrue(lisi.waitForExistence(timeout: 5), "synthetic 李四 row must render")
        setAppearance(.light)
        attach("recordings-list-light")
        setAppearance(.dark)
        attach("recordings-list-dark")
    }

    // MARK: - Build 54 conference recording surfaces (offline synthetic fixture)

    /// Hosted conference with a server-confirmed conference recording: red
    /// indicator, stop control, member rows, plus the conference row in the
    /// recordings list. Fictional 张三/李四 only; no gateway, credentials or
    /// audio.
    func testConferenceRecordingSurfacesFixture() throws {
        launchChinese(["-callrelayConferenceRecordingPreview"])
        let indicator = app.descendants(matching: .any)["call.recordingIndicator"].firstMatch
        XCTAssertTrue(indicator.waitForExistence(timeout: 12),
                      "conference recording indicator must render")
        XCTAssertTrue(button(containing: "停止录音").waitForExistence(timeout: 5),
                      "conference record control must offer stop while recording")
        XCTAssertTrue(app.staticTexts["多方会议 · 2 方"].waitForExistence(timeout: 5),
                      "conference member header must render")
        setAppearance(.light)
        attach("conference-recording-incall-light")
        setAppearance(.dark)
        attach("conference-recording-incall-dark")

        // The recorded conference appears as one row with its member peer
        // label and the conference badge.
        app.terminate()
        launchChinese(["-callrelayRecordingListPreview"])
        XCTAssertTrue(app.selectSection("最近通话"), "Recents destination must exist")
        let entry = app.buttons["recentsRecordings"]
        XCTAssertTrue(entry.waitForExistence(timeout: 8), "recordings toolbar entry must exist")
        entry.tap()
        XCTAssertTrue(app.navigationBars["通话录音"].waitForExistence(timeout: 8),
                      "recordings list must open")
        let conferenceRow = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS %@", "张三、李四")).firstMatch
        XCTAssertTrue(conferenceRow.waitForExistence(timeout: 5),
                      "conference recording row must render")
        // Capture BOTH appearances explicitly (build 54 shipped a "light"
        // capture taken under dark after an earlier appearance switch).
        setAppearance(.light)
        attach("recordings-list-conference-light")
        setAppearance(.dark)
        attach("recordings-list-conference-dark")
    }
}
