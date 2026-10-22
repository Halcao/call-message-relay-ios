import XCTest

/// Screenshot-only review of the native gateway-administrator console.
///
/// Everything renders from the synthetic launch-argument fixture: no gateway,
/// no login, no Keychain, no credentials. The test also asserts that normal
/// Back navigation works (the console is pushed inside the Settings stack, so
/// a nested NavigationStack would break the system Back button).
final class ManagementVisualUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUpWithError() throws {
        continueAfterFailure = false
        app = XCUIApplication()
    }

    override func tearDownWithError() throws {
        app = nil
        XCUIDevice.shared.appearance = .light
    }

    private func launch(_ extra: [String] = []) {
        app.launchArguments = [
            "-callrelayUITestReset",
            "-callrelayPairedFixture",
            "-callrelayManagementPreview",
            "-AppleLanguages", "(zh-Hans)",
            "-AppleLocale", "zh_CN",
        ] + extra
        app.launch()
    }

    private func attach(_ name: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    private func openSettingsTab() {
        // Adaptive: a bottom tab on iPhone, a sidebar row on iPad.
        let selected = app.selectSection("设置", timeout: 20)
        XCTAssertTrue(selected, "settings section missing")
    }

    /// Scrolls the current form until `element` exists and is hittable.
    /// SwiftUI lists virtualize their cells, so an off-screen row is not in
    /// the accessibility tree at all — a bounded scroll is the honest way to
    /// reach it.
    @discardableResult
    private func scrollTo(_ element: XCUIElement, attempts: Int = 10) -> Bool {
        for attempt in 0..<attempts {
            if element.exists, element.isHittable { return true }
            // Alternate directions: a row may be above or below the current
            // viewport, and SwiftUI only materializes visible cells.
            if attempt < attempts / 2 {
                app.swipeUp()
            } else {
                app.swipeDown()
            }
        }
        return element.exists && element.isHittable
    }

    private func openManagement() {
        let entry = app.buttons["managementEntry"].firstMatch
        if !entry.waitForExistence(timeout: 6) {
            // The entry sits below the fold: scroll the settings form to it.
            XCTAssertTrue(scrollTo(entry), "management entry missing after scrolling")
        }
        XCTAssertTrue(scrollTo(entry), "management entry not hittable")
        entry.tap()
    }

    private func goBack() {
        let back = app.navigationBars.buttons.element(boundBy: 0)
        XCTAssertTrue(back.waitForExistence(timeout: 5), "back button missing")
        back.tap()
    }

    func testManagementConsoleMainFlows() throws {
        launch()
        openSettingsTab()
        attach("01-settings-entry")
        openManagement()

        // Root console: FUNCTIONS first (root review), summary after.
        let linesLink = app.buttons["线路"].firstMatch
        XCTAssertTrue(scrollTo(linesLink), "console root did not render its functions")
        attach("02-management-root")

        // Every section renders, and Back returns to the console root.
        let sections: [(label: String, marker: String)] = [
            ("线路", "managementLinesSave"),
            ("配对密钥", "managementKeyCreate"),
            ("设备", "managementDeviceRevoke"),
            ("通话与会议", "managementCallHangup"),
            ("语音留言", "managementVoicemailDelete"),
            ("录音策略", "managementRecordingToggle"),
            ("网关设置", "managementSettingsSave"),
            ("诊断与恢复", "managementRecoverLine"),
        ]
        for (index, section) in sections.enumerated() {
            let link = app.buttons[section.label].firstMatch
            XCTAssertTrue(scrollTo(link), "missing section \(section.label)")
            link.tap()
            XCTAssertTrue(app.navigationBars[section.label].waitForExistence(timeout: 8),
                          "section \(section.label) did not open")
            attach(String(format: "%02d-management-%@", index + 3, section.label))
            goBack()
            let rootBar = app.navigationBars["网关管理"]
            if !rootBar.waitForExistence(timeout: 6) {
                print("=== BACK DID NOT REACH THE CONSOLE ROOT from \(section.label) ===")
                print(app.debugDescription)
            }
            XCTAssertTrue(rootBar.exists, "Back did not return to the console root from \(section.label)")
        }

        // A line EDITOR screen (rows are pushed inside the lines list).
        let lines = app.buttons["线路"].firstMatch
        XCTAssertTrue(scrollTo(lines), "lines entry missing")
        lines.tap()
        XCTAssertTrue(app.navigationBars["线路"].waitForExistence(timeout: 8), "lines list did not open")
        let firstLine = app.cells.element(boundBy: 0)
        XCTAssertTrue(firstLine.waitForExistence(timeout: 8), "no line row to edit")
        firstLine.tap()
        XCTAssertTrue(app.buttons["managementLinesSave"].waitForExistence(timeout: 8)
                      || app.navigationBars.buttons["保存"].waitForExistence(timeout: 4),
                      "line editor did not open")
        attach("11-management-line-editor")
        goBack()
        XCTAssertTrue(app.navigationBars["线路"].waitForExistence(timeout: 8), "editor Back failed")
        goBack()
        XCTAssertTrue(app.navigationBars["网关管理"].waitForExistence(timeout: 8),
                      "lines Back did not return to the console root")

        // The account block (and 退出管理) lives at the BOTTOM: the list must
        // be scrolled to it, never assumed hittable at the top.
        let signOut = app.buttons["managementSignOut"].firstMatch
        XCTAssertTrue(scrollTo(signOut), "logout not reachable by scrolling to the bottom")
        attach("12-management-account-bottom")
        signOut.tap()
        // The confirmation dialog is a real dialog: confirm it and land on the
        // login prompt (which is the honest signed-out state).
        let confirm = app.buttons["退出管理"].firstMatch
        if confirm.waitForExistence(timeout: 4) {
            confirm.tap()
        }
        XCTAssertTrue(app.buttons["managementLogin"].waitForExistence(timeout: 10),
                      "signing out did not present the administrator login")
        attach("13-management-login")

        // Back from the console goes to Settings (no nested stack swallowed it).
        goBack()
        XCTAssertTrue(app.navigationBars["设置"].waitForExistence(timeout: 8),
                      "Back from the console did not return to Settings")
        attach("14-settings-after-back")
    }

    /// Signed-out fixture: the administrator login must render with an ENABLED
    /// form (a real bound gateway origin) instead of a dead "address
    /// unavailable" state. No password is typed, no network is used.
    func testManagementLoginFormIsUsableWhenSignedOut() throws {
        app.launchArguments = [
            "-callrelayUITestReset",
            "-callrelayPairedFixture",
            "-callrelayManagementPreview",
            "-callrelayManagementLoginPreview",
            "-AppleLanguages", "(zh-Hans)",
            "-AppleLocale", "zh_CN",
        ]
        app.launch()
        openSettingsTab()
        openManagement()
        let field = app.textFields["managementUsername"]
        XCTAssertTrue(field.waitForExistence(timeout: 10), "username field missing")
        XCTAssertTrue(field.isEnabled, "the login form must be enabled with a bound origin")
        XCTAssertTrue(app.secureTextFields["managementPassword"].isEnabled,
                      "the password field must be enabled with a bound origin")
        XCTAssertTrue(app.buttons["managementLogin"].exists, "login button missing")
        attach("15-management-login-signed-out")
    }

    /// An UNPAIRED install shows onboarding (no settings surface at all): the
    /// capability gate itself is covered by the unit tests, so this fixture
    /// only records the launch state for the review.
    func testUnpairedOnboardingStillRenders() throws {
        app.launchArguments = [
            "-callrelayUITestReset",
            "-AppleLanguages", "(zh-Hans)",
            "-AppleLocale", "zh_CN",
        ]
        app.launch()
        let onboarding = app.staticTexts.element(boundBy: 0)
        XCTAssertTrue(onboarding.waitForExistence(timeout: 20), "app rendered nothing")
        attach("12-unpaired-launch")
    }
}
