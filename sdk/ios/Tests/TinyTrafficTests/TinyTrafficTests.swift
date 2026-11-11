import XCTest
@testable import TinyTraffic

final class TinyTrafficTests: XCTestCase {
    private final class Box: @unchecked Sendable {
        let lock = NSLock()
        var requests: [[String: Any]] = []
        var outcome: TinyTraffic.Delivery = .accepted
        func add(_ r: URLRequest) {
            lock.lock(); defer { lock.unlock() }
            if let body = r.httpBody, let json = try? JSONSerialization.jsonObject(with: body) as? [String: Any] { requests.append(json) }
        }
        var all: [[String: Any]] { lock.lock(); defer { lock.unlock() }; return requests }
    }

    private func makeDefaults() -> UserDefaults {
        let name = "tinytraffic.test.\(UUID().uuidString)"
        let d = UserDefaults(suiteName: name)!
        d.removePersistentDomain(forName: name)
        return d
    }

    private func wait(_ cond: @escaping () -> Bool, timeout: TimeInterval = 3) {
        let end = Date().addingTimeInterval(timeout)
        while !cond() && Date() < end { RunLoop.current.run(until: Date().addingTimeInterval(0.01)) }
    }

    func testBatchesEventsWithContext() {
        let box = Box()
        let tt = TinyTraffic()
        let defaults = makeDefaults()
        tt.configure(.init(endpoint: URL(string: "https://stats.example.com")!, appId: "com.example.app", channel: "appstore", batchSize: 3),
                     defaults: defaults,
                     transport: { req, done in
                         XCTAssertEqual(req.url?.absoluteString, "https://stats.example.com/api/app/event")
                         box.add(req); done(box.outcome)
                     })
        // configure records app_open; two more fill the batch.
        tt.screen("closet")
        tt.track("outfit_saved", screen: "studio", props: ["items": 4])
        wait { !box.all.isEmpty }

        guard let body = box.all.first else { return XCTFail("nothing sent") }
        XCTAssertEqual(body["app_id"] as? String, "com.example.app")
        XCTAssertEqual(body["channel"] as? String, "appstore")
        XCTAssertFalse((body["device_id"] as? String ?? "").isEmpty)
        XCTAssertFalse((body["session_id"] as? String ?? "").isEmpty)
        XCTAssertNotNil(body["device_model"])
        let events = body["events"] as? [[String: Any]] ?? []
        XCTAssertEqual(events.map { $0["name"] as? String }, ["app_open", "screen_view", "outfit_saved"])
        XCTAssertEqual(events[1]["screen"] as? String, "closet")
        XCTAssertEqual((events[2]["props"] as? [String: String])?["items"], "4")
        XCTAssertNil(events[0]["session"], "the per-event session is internal")

        // Same device ID next time.
        let id = body["device_id"] as? String
        XCTAssertEqual(defaults.string(forKey: "tinytraffic.device_id"), id)
    }

    func testKeepsEventsWhenOfflineAndRetries() {
        let box = Box()
        box.outcome = .retry
        let tt = TinyTraffic()
        let defaults = makeDefaults()
        tt.configure(.init(endpoint: URL(string: "https://stats.example.com")!, appId: "com.example.app", batchSize: 1),
                     defaults: defaults,
                     transport: { req, done in box.add(req); done(box.outcome) })
        wait { box.all.count >= 1 }
        XCTAssertEqual((defaults.array(forKey: "tinytraffic.queue") ?? []).count, 1, "queued events survive a failed send")

        box.lock.lock(); box.outcome = .accepted; box.lock.unlock()
        tt.flush()
        wait { (defaults.array(forKey: "tinytraffic.queue") ?? []).isEmpty }
        XCTAssertTrue((defaults.array(forKey: "tinytraffic.queue") ?? []).isEmpty)
    }

    func testDisabledRecordsNothingAndTurningOffDropsTheQueue() {
        let box = Box()
        let tt = TinyTraffic()
        let defaults = makeDefaults()
        // Offline, so what is queued stays queued.
        box.outcome = .retry
        tt.configure(.init(endpoint: URL(string: "https://stats.example.com")!, appId: "com.example.app", enabled: false, batchSize: 100, flushInterval: 3600),
                     defaults: defaults,
                     transport: { req, done in box.add(req); done(box.outcome) })
        tt.screen("closet")
        XCTAssertFalse(tt.isEnabled)
        XCTAssertTrue((defaults.array(forKey: "tinytraffic.queue") ?? []).isEmpty, "nothing is recorded before consent")

        tt.isEnabled = true   // consent: starts with app_open
        tt.screen("closet")
        wait { (defaults.array(forKey: "tinytraffic.queue") ?? []).count == 2 }
        XCTAssertEqual((defaults.array(forKey: "tinytraffic.queue") ?? []).count, 2)

        tt.isEnabled = false  // opted out: unsent events are discarded
        XCTAssertTrue((defaults.array(forKey: "tinytraffic.queue") ?? []).isEmpty)
    }

    func testDebugBuildsSendNothing() {
        let box = Box()
        let tt = TinyTraffic()
        tt.configure(.init(endpoint: URL(string: "https://stats.example.com")!, appId: "com.example.app", debug: true, batchSize: 1),
                     defaults: makeDefaults(),
                     transport: { req, done in box.add(req); done(.accepted) })
        tt.screen("closet")
        tt.flush()
        RunLoop.current.run(until: Date().addingTimeInterval(0.2))
        XCTAssertTrue(box.all.isEmpty)
    }
}
