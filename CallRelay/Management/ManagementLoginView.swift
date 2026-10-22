import SwiftUI

/// Administrator password login for the gateway console. The password exists
/// only for the duration of the request; only the gateway-issued session is
/// kept, in this device's Keychain (never synced to iCloud).
struct ManagementLoginView: View {
    @ObservedObject var center: AdminCenter
    @EnvironmentObject private var model: AppModel
    @State private var username = ""
    @State private var password = ""

    var body: some View {
        Form {
            Section {
                TextField("用户名", text: $username)
                    .textContentType(.username)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .accessibilityIdentifier("managementUsername")
                SecureField("密码", text: $password)
                    .textContentType(.password)
                    .accessibilityIdentifier("managementPassword")
                Button {
                    Task { await signIn() }
                } label: {
                    HStack {
                        Text("登录")
                        if center.isSigningIn {
                            Spacer()
                            ProgressView()
                        }
                    }
                }
                .disabled(!canSignIn)
                .accessibilityIdentifier("managementLogin")
            } header: {
                Text("管理员登录")
            } footer: {
                Text("登录仅用于网关管理。")
            }

            if let error = center.errorText {
                Section {
                    Text(error).font(.caption).foregroundStyle(.red)
                }
            }

            if loginOrigin == nil {
                Section {
                    Text("当前网关地址不可用，无法登录管理控制台。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .onDisappear { password = "" }
    }

    /// The console's own bound origin is authoritative: it is the gateway the
    /// administrator session belongs to, and it stays valid even when the live
    /// pairing state is still resolving.
    private var loginOrigin: GatewayOrigin? { center.origin ?? model.gatewayOrigin }

    private var canSignIn: Bool {
        !center.isSigningIn
            && !username.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && !password.isEmpty
            && loginOrigin != nil
    }

    private func signIn() async {
        guard let origin = loginOrigin else { return }
        let gatewayId = center.gatewayId ?? model.currentIntentPrincipal ?? ""
        let attempt = password
        defer {
            // The password exists only for this one attempt: never keep it in
            // view state after the request returns (success or failure).
            password = ""
        }
        _ = await center.signIn(
            origin: origin,
            gatewayId: gatewayId,
            username: username.trimmingCharacters(in: .whitespacesAndNewlines),
            password: attempt
        )
    }
}
