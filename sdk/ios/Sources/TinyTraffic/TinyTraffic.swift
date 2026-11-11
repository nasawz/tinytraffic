import Foundation
#if canImport(UIKit)
import UIKit
#endif

/// Privacy-friendly analytics for apps, sending events to a tinytraffic server.
///
///     TinyTraffic.shared.configure(endpoint: "https://stats.example.com", appId: "com.example.app")
///     TinyTraffic.shared.screen("closet")
///     TinyTraffic.shared.track("outfit_saved", props: ["items": 4])
///
/// - The device ID is a random UUID kept in `UserDefaults`. It is removed with
///   the app, so a reinstall counts as a new install. It never leaves the
///   server as-is (the server hashes it with the app ID) and nothing in it
///   identifies the person or links apps together.
/// - Events are queued, sent in batches and kept across launches until the
///   server has accepted them.
public final class TinyTraffic: @unchecked Sendable {
    public static let shared = TinyTraffic()

    public struct Configuration {
        /// Base URL of the tinytraffic server, e.g. `https://stats.example.com`.
        public var endpoint: URL
        /// The app ID the site was registered with, usually the bundle ID.
        public var appId: String
        /// Distribution channel, e.g. `appstore` or `testflight`.
        public var channel: String?
        /// Development builds: events are collected nowhere and sent nowhere.
        public var debug: Bool
        /// A session ends after this long in the background.
        public var sessionTimeout: TimeInterval
        /// Events are sent once this many are queued, or after `flushInterval`.
        public var batchSize: Int
        public var flushInterval: TimeInterval

        public init(endpoint: URL, appId: String, channel: String? = nil, debug: Bool = false,
                    sessionTimeout: TimeInterval = 30 * 60, batchSize: Int = 20, flushInterval: TimeInterval = 10) {
            self.endpoint = endpoint
            self.appId = appId
            self.channel = channel
            self.debug = debug
            self.sessionTimeout = sessionTimeout
            self.batchSize = batchSize
            self.flushInterval = flushInterval
        }
    }

    /// Sends one JSON body and reports whether the server took it (`.accepted`),
    /// refused it for good (`.rejected`, so retrying is pointless) or could not
    /// be reached (`.retry`). Replaced in tests.
    public enum Delivery: Sendable { case accepted, rejected, retry }
    public typealias Transport = @Sendable (URLRequest, @escaping @Sendable (Delivery) -> Void) -> Void

    private let queue = DispatchQueue(label: "tinytraffic")
    private var config: Configuration?
    private var transport: Transport = TinyTraffic.urlSessionTransport
    private var defaults: UserDefaults = .standard
    private var pending: [[String: Any]] = []
    private var sessionId = UUID().uuidString
    private var backgroundedAt: Date?
    private var flushScheduled = false
    private var enabled = true
    private var sending = false
    private var context: [String: Any] = [:]
    private var observers: [NSObjectProtocol] = []

    private static let queueKey = "tinytraffic.queue"
    private static let deviceKey = "tinytraffic.device_id"
    /// Events older than the server accepts are not worth keeping.
    private static let maxQueued = 500

    public init() {}

    /// Call once at launch. Starts a session and records `app_open`.
    public func configure(_ configuration: Configuration, defaults: UserDefaults = .standard, transport: Transport? = nil) {
        queue.sync {
            self.config = configuration
            self.defaults = defaults
            if let transport { self.transport = transport }
            self.context = Self.deviceContext()
            self.pending = (defaults.array(forKey: Self.queueKey) as? [[String: Any]]) ?? []
            self.sessionId = UUID().uuidString
            self.backgroundedAt = nil
        }
        observeLifecycle()
        track("app_open")
    }

    public func configure(endpoint: String, appId: String, channel: String? = nil, debug: Bool = false) {
        guard let url = URL(string: endpoint) else { return }
        configure(Configuration(endpoint: url, appId: appId, channel: channel, debug: debug))
    }

    /// Records that a screen was shown. Shows up under Pages.
    public func screen(_ name: String) {
        record(name: "screen_view", screen: name, props: nil)
    }

    /// Records a custom event. Shows up under Goals. Props are flattened to strings.
    public func track(_ name: String, screen: String? = nil, props: [String: Any]? = nil) {
        record(name: name, screen: screen, props: props)
    }

    /// Turn collection on or off, e.g. with the user's consent. While off, nothing is queued or sent.
    public var isEnabled: Bool {
        get { queue.sync { enabled } }
        set { queue.sync { enabled = newValue } }
    }

    /// Sends everything queued now (e.g. before the app suspends).
    public func flush() {
        queue.async { self.sendLocked() }
    }

    // MARK: - Internals

    private func record(name: String, screen: String?, props: [String: Any]?) {
        queue.async {
            guard let config = self.config, !config.debug, self.enabled else { return }
            var event: [String: Any] = ["name": name, "ts": Int64(Date().timeIntervalSince1970 * 1000), "session": self.sessionId]
            if let screen { event["screen"] = screen }
            if let props, !props.isEmpty {
                event["props"] = props.mapValues { "\($0)" }
            }
            self.pending.append(event)
            if self.pending.count > Self.maxQueued { self.pending.removeFirst(self.pending.count - Self.maxQueued) }
            self.persistLocked()
            if self.pending.count >= config.batchSize {
                self.sendLocked()
            } else if !self.flushScheduled {
                self.flushScheduled = true
                self.queue.asyncAfter(deadline: .now() + config.flushInterval) {
                    self.flushScheduled = false
                    self.sendLocked()
                }
            }
        }
    }

    /// Runs on `queue`. Sends one batch; on success, goes on with the rest.
    private func sendLocked() {
        guard let config, !config.debug, !sending, !pending.isEmpty else { return }
        let batch = Array(pending.prefix(100))
        // Events carry the session they happened in, so one request can span
        // sessions; the server wants one session per request.
        let bySession = batch.first?["session"] as? String
        let sameSession = batch.prefix { ($0["session"] as? String) == bySession }
        let events = Array(sameSession)
        guard let body = Self.body(config: config, context: context, deviceId: deviceId(), session: bySession ?? sessionId, events: events) else {
            pending.removeFirst(events.count)
            return
        }
        var request = URLRequest(url: config.endpoint.appendingPathComponent("api/app/event"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = body
        request.timeoutInterval = 15
        sending = true
        transport(request) { result in
            self.queue.async {
                self.sending = false
                switch result {
                case .accepted, .rejected:
                    self.pending.removeFirst(min(events.count, self.pending.count))
                    self.persistLocked()
                    if !self.pending.isEmpty { self.sendLocked() }
                case .retry:
                    // Try again on the next event, launch or foreground.
                    break
                }
            }
        }
    }

    static func body(config: Configuration, context: [String: Any], deviceId: String, session: String, events: [[String: Any]]) -> Data? {
        var payload = context
        payload["app_id"] = config.appId
        payload["device_id"] = deviceId
        payload["session_id"] = session
        if let channel = config.channel { payload["channel"] = channel }
        payload["events"] = events.map { e -> [String: Any] in
            var e = e
            e.removeValue(forKey: "session")
            return e
        }
        return try? JSONSerialization.data(withJSONObject: payload)
    }

    private func persistLocked() {
        defaults.set(pending, forKey: Self.queueKey)
    }

    private func deviceId() -> String {
        if let id = defaults.string(forKey: Self.deviceKey), !id.isEmpty { return id }
        let id = UUID().uuidString
        defaults.set(id, forKey: Self.deviceKey)
        return id
    }

    private func observeLifecycle() {
        #if canImport(UIKit) && !os(watchOS)
        guard observers.isEmpty else { return }
        let nc = NotificationCenter.default
        observers = [
            nc.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: nil) { [weak self] _ in
                self?.didEnterBackground()
            },
            nc.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: nil) { [weak self] _ in
                self?.willEnterForeground()
            },
        ]
        #endif
    }

    func didEnterBackground() {
        queue.async { self.backgroundedAt = Date() }
        #if canImport(UIKit) && !os(watchOS)
        // Ask for a little time to deliver what is queued.
        var task = UIBackgroundTaskIdentifier.invalid
        task = UIApplication.shared.beginBackgroundTask(withName: "tinytraffic.flush") {
            UIApplication.shared.endBackgroundTask(task)
        }
        queue.async {
            self.sendLocked()
            self.queue.asyncAfter(deadline: .now() + 5) { UIApplication.shared.endBackgroundTask(task) }
        }
        #else
        flush()
        #endif
    }

    func willEnterForeground() {
        queue.async {
            guard let config = self.config, let left = self.backgroundedAt else { return }
            self.backgroundedAt = nil
            if Date().timeIntervalSince(left) >= config.sessionTimeout {
                self.sessionId = UUID().uuidString
                self.queue.async { self.record(name: "app_open", screen: nil, props: nil) }
            }
            self.sendLocked()
        }
    }

    // MARK: - Device facts

    static func deviceContext() -> [String: Any] {
        let info = Bundle.main.infoDictionary ?? [:]
        var c: [String: Any] = [
            "app_version": info["CFBundleShortVersionString"] as? String ?? "",
            "build": info["CFBundleVersion"] as? String ?? "",
            "device_model": hardwareModel(),
            "locale": Locale.preferredLanguages.first ?? Locale.current.identifier,
        ]
        #if canImport(UIKit) && !os(watchOS)
        let device = UIDevice.current
        c["os"] = device.systemName == "iOS" && device.userInterfaceIdiom == .pad ? "iPadOS" : device.systemName
        c["os_version"] = device.systemVersion
        c["device_type"] = device.userInterfaceIdiom == .pad ? "tablet" : "phone"
        c["screen_width"] = Int(UIScreen.main.bounds.width)
        #else
        c["os"] = "macOS"
        let v = ProcessInfo.processInfo.operatingSystemVersion
        c["os_version"] = "\(v.majorVersion).\(v.minorVersion).\(v.patchVersion)"
        c["device_type"] = "desktop"
        #endif
        return c
    }

    /// The model identifier, e.g. `iPhone17,1`.
    static func hardwareModel() -> String {
        var size = 0
        sysctlbyname("hw.machine", nil, &size, nil, 0)
        var machine = [CChar](repeating: 0, count: max(size, 1))
        sysctlbyname("hw.machine", &machine, &size, nil, 0)
        return String(cString: machine)
    }

    static let urlSessionTransport: Transport = { request, done in
        URLSession.shared.dataTask(with: request) { _, response, error in
            guard error == nil, let http = response as? HTTPURLResponse else { return done(.retry) }
            switch http.statusCode {
            case 200..<300: done(.accepted)
            case 429, 500..<600: done(.retry)
            default: done(.rejected)
            }
        }.resume()
    }
}
