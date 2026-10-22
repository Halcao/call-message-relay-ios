import Foundation
import Network
import UIKit

/// Publishes coarse reachability so reconnect loops can retry immediately when
/// the network returns instead of waiting out the full backoff. It reports
/// only "is there a usable path", never the user's location or network name.
@MainActor
final class NetworkMonitor: ObservableObject {
    /// Shared instance so lower layers (e.g. the call coordinator's
    /// direct-path probe gate) can read the current path type without
    /// threading references through every initializer.
    static let shared = NetworkMonitor()

    @Published private(set) var isReachable = true
    @Published private(set) var isConstrained = false
    private let monitor = NWPathMonitor()
    private var started = false
    /// True while the current path includes Wi-Fi (a direct LAN route to the
    /// gateway is plausible). Cellular-only paths report false: with no
    /// configured ICE servers there is no direct path to discover.
    @Published private(set) var currentPathUsesWiFi = false
    /// Monotonic identity of the CURRENT network path. It changes only when
    /// the path really changed (status, constrained flag or interface set), so
    /// cached reachability/candidate evidence gathered on the old path can be
    /// invalidated deterministically (build 68: a Wi-Fi→cellular switch must
    /// not let a stale direct candidate look fresh).
    @Published private(set) var pathGeneration: UInt64 = 0
    private var lastPathSignature = ""

    func start() {
        guard !started else { return }
        started = true
        monitor.pathUpdateHandler = { [weak self] path in
            Task { @MainActor in
                guard let self else { return }
                self.isReachable = path.status == .satisfied
                self.isConstrained = path.isConstrained
                self.currentPathUsesWiFi = path.availableInterfaces.contains {
                    $0.type == .wifi
                }
                let signature = Self.signature(of: path)
                if signature != self.lastPathSignature {
                    self.lastPathSignature = signature
                    self.pathGeneration &+= 1
                }
            }
        }
        monitor.start(queue: DispatchQueue(label: "callrelay.networkmonitor"))
    }

    func stop() {
        guard started else { return }
        started = false
        monitor.cancel()
    }

    /// Stable, privacy-safe description of the path shape (no SSID, no
    /// addresses): enough to notice a real switch, nothing that identifies a
    /// network by name.
    private static func interfaceLabel(_ type: NWInterface.InterfaceType) -> String {
        switch type {
        case .wifi: return "wifi"
        case .cellular: return "cell"
        case .wiredEthernet: return "wired"
        case .loopback: return "lo"
        case .other: return "other"
        @unknown default: return "unknown"
        }
    }
    private static func signature(of path: NWPath) -> String {
        let interfaces = path.availableInterfaces
            .map { "\(Self.interfaceLabel($0.type)):\($0.index)" }
            .sorted()
            .joined(separator: ",")
        return "\(path.status == .satisfied ? 1 : 0)|\(path.isConstrained ? 1 : 0)|\(interfaces)"
    }

    /// Fires when the app re-enters the foreground. Callers use it to reconcile
    /// state immediately after iOS may have suspended the app.
    static var foregroundNotification: Notification.Name {
        UIApplication.willEnterForegroundNotification
    }
}
