import XCTest
import Darwin
@testable import LightpinkyCore

final class ReadRecoveryTests: XCTestCase {
  private final class Counter {
    private let lock = NSLock()
    private var value = 0
    func next() -> Int { lock.lock(); defer { lock.unlock() }; value += 1; return value }
    var count: Int { lock.lock(); defer { lock.unlock() }; return value }
  }

  func testFastReadDoesNotStartAReplacement() throws {
    let calls = Counter()
    XCTAssertFalse(try ReadRecovery<Bool>("test").read {
      _ = calls.next()
      return false
    })
    XCTAssertEqual(calls.count, 1)
  }

  func testStalledReadIsCanceledAndReplaced() throws {
    let calls = Counter(), canceled = expectation(description: "first task canceled")
    let value = try ReadRecovery<Bool>("test").read(firstTimeout: 0.02, replacementTimeout: 1) {
      if calls.next() == 1 {
        return try await withTaskCancellationHandler(operation: {
          try await Task.sleep(nanoseconds: 10_000_000_000)
          return true
        }, onCancel: { canceled.fulfill() })
      }
      return false
    }
    XCTAssertFalse(value)
    XCTAssertEqual(calls.count, 2)
    wait(for: [canceled], timeout: 1)
  }

  func testLateCanceledReplyCannotSatisfyTheNextObservation() throws {
    let calls = Counter(), reader = ReadRecovery<Bool>("test")
    XCTAssertFalse(try reader.read(firstTimeout: 0.02, replacementTimeout: 1) {
      if calls.next() == 1 {
        // Deliberately ignores cancellation, like some framework callbacks.
        return await withCheckedContinuation { continuation in
          DispatchQueue.global().asyncAfter(deadline: .now() + 0.1) { continuation.resume(returning: true) }
        }
      }
      return false
    })
    XCTAssertFalse(try reader.read(firstTimeout: 1, replacementTimeout: 1) {
      await withCheckedContinuation { continuation in
        DispatchQueue.global().asyncAfter(deadline: .now() + 0.2) { continuation.resume(returning: false) }
      }
    })
  }

  func testUncooperativeCallsCannotAccumulateAcrossObservations() {
    let calls = Counter(), reader = ReadRecovery<Bool>("test")
    let finished = expectation(description: "abandoned calls eventually finish")
    finished.expectedFulfillmentCount = 2
    XCTAssertThrowsError(try reader.read(firstTimeout: 0.02, replacementTimeout: 0.02) {
      _ = calls.next()
      return await withCheckedContinuation { continuation in
        DispatchQueue.global().asyncAfter(deadline: .now() + 0.2) {
          continuation.resume(returning: true)
          finished.fulfill()
        }
      }
    })
    XCTAssertThrowsError(try reader.read {
      _ = calls.next()
      return false
    })
    XCTAssertEqual(calls.count, 2)
    wait(for: [finished], timeout: 1)
    // The slots must become reusable once the framework callbacks finish.
    let deadline = ProcessInfo.processInfo.systemUptime + 1
    var recovered = false
    repeat {
      recovered = (try? reader.read { true }) == true
      if !recovered { Thread.sleep(forTimeInterval: 0.001) }
    } while !recovered && ProcessInfo.processInfo.systemUptime < deadline
    XCTAssertTrue(recovered)
  }

  func testTransportFailureRetriesButDeviceRefusalDoesNot() throws {
    let calls = Counter()
    XCTAssertTrue(try ReadRecovery<Bool>("test").read {
      if calls.next() == 1 { throw CoreConnectionFailure.disconnected("test") }
      return true
    })
    XCTAssertEqual(calls.count, 2)
    let refused = Counter()
    XCTAssertThrowsError(try ReadRecovery<Bool>("test").read {
      _ = refused.next()
      throw PhoneError("Device refused the request")
    })
    XCTAssertEqual(refused.count, 1)
  }
}

final class ConnectionRecoveryTests: XCTestCase {
  private func keypad(field: String = "Passcode field, 0 of 6 values entered",
                      focused: Bool = true) -> [[String: Any]] {
    [["Label": "Enter Passcode"],
     ["Label": field, "States": focused ? ["focused", "password"] : ["password"]]]
      + (0...9).map { ["Label": String($0)] }
  }

  func testKeypadRequiresKnownLockStateFocusAndEveryDigit() throws {
    XCTAssertEqual(try Phone.unlockReadiness(locked: true, elements: keypad(), codeLength: 6), .passcode)
    XCTAssertEqual(try Phone.unlockReadiness(locked: false, elements: keypad(), codeLength: 6), .unlocked)
    XCTAssertEqual(try Phone.unlockReadiness(locked: nil, elements: keypad(), codeLength: 6), .waiting)
    XCTAssertEqual(try Phone.unlockReadiness(locked: true, elements: keypad(focused: false), codeLength: 6), .waiting)
    XCTAssertEqual(try Phone.unlockReadiness(locked: true, elements: Array(keypad().dropLast()), codeLength: 6), .waiting)
  }

  func testKeypadRejectsPartialEntryWrongLengthAndLockout() {
    for elements in [
      keypad(field: "Passcode field, 1 of 6 values entered"),
      keypad(field: "Passcode field, 0 of 4 values entered"),
      keypad() + [["Label": "iPhone Unavailable"]],
      keypad() + [["Label": "Try again in 1 minute"]],
    ] {
      XCTAssertThrowsError(try Phone.unlockReadiness(locked: true, elements: elements, codeLength: 6))
    }
  }

  func testObservationRecoversFromOneTransportFailure() throws {
    for failure in [CoreConnectionFailure.timedOut("lock state"), .disconnected("stream")] {
      var calls = 0
      let value = try CoreConnectionFailure.retryRead {
        calls += 1
        if calls == 1 { throw failure }
        return ["Locked": true, "ScreenOn": false]
      }
      XCTAssertEqual(calls, 2)
      XCTAssertEqual(value, ["Locked": true, "ScreenOn": false])
    }
  }

  func testSecondFailureStopsRatherThanLooping() {
    var calls = 0
    XCTAssertThrowsError(try CoreConnectionFailure.retryRead { () -> Bool in
      calls += 1
      throw CoreConnectionFailure.timedOut("stream")
    }) { XCTAssertTrue($0 is CoreConnectionFailure) }
    XCTAssertEqual(calls, 2)
  }

  func testDeviceRefusalsAndMalformedResponsesAreNotRetried() {
    for message in ["CoreDevice rejected the service request", "Invalid application catalog"] {
      var calls = 0
      XCTAssertThrowsError(try CoreConnectionFailure.retryRead { () -> Bool in
        calls += 1
        throw PhoneError(message)
      }) { XCTAssertEqual($0.localizedDescription, message) }
      XCTAssertEqual(calls, 1)
    }
  }

  func testSuccessfulObservationRunsOnce() throws {
    var calls = 0
    XCTAssertFalse(try CoreConnectionFailure.retryRead { calls += 1; return false })
    XCTAssertEqual(calls, 1)
  }

  func testUnlockConfirmationStillReadsAfterTimeoutExhaustsPollingDeadline() throws {
    var time = 0.0, reads = 0, reconnects = 0
    XCTAssertTrue(try Phone.confirmUnlock(locked: {
      reads += 1
      if reads == 1 {
        time += 20
        throw CoreConnectionFailure.timedOut("lock state")
      }
      return false
    }, reconnect: { reconnects += 1 }, now: { time }, pause: { time += 0.3 }))
    XCTAssertEqual(reads, 2)
    XCTAssertEqual(reconnects, 1)
  }

  func testUnlockConfirmationDoesNotReconnectTwice() {
    var time = 0.0, reads = 0, reconnects = 0
    XCTAssertThrowsError(try Phone.confirmUnlock(locked: {
      reads += 1
      throw CoreConnectionFailure.disconnected("connection")
    }, reconnect: { reconnects += 1 }, now: { time }, pause: { time += 0.3 }))
    XCTAssertEqual(reads, 2)
    XCTAssertEqual(reconnects, 1)
  }

  func testStillLockedConfirmationExpiresWithoutReconnecting() throws {
    var time = 0.0, reconnects = 0
    XCTAssertFalse(try Phone.confirmUnlock(locked: { true }, reconnect: { reconnects += 1 },
                                          now: { time }, pause: { time += 1 }))
    XCTAssertEqual(time, 5)
    XCTAssertEqual(reconnects, 0)
  }
}

final class AccessibilitySnapshotTests: XCTestCase {
  private let app: [String: Any] = ["pid": 10, "id": 1]
  private let child: [String: Any] = ["pid": 10, "id": 2]

  private var front: [String: Any] { ["type": 4, "result": app] }
  private var root: [String: Any] { ["type": 5, "object": app, "result": ["9": [child]]] }
  private var leaf: [String: Any] { ["type": 5, "object": child, "result": ["33": "Enter Passcode"]] }

  func testOrdersUnorderedSnapshotFromForegroundRoot() throws {
    let result = try XCTUnwrap(DTXSession.orderedTree([leaf, front, root], front: front))
    XCTAssertEqual(result.count, 3)
    XCTAssertEqual((result[1]["object"] as? [String: Int])?["id"], 1)
    XCTAssertEqual((result[2]["object"] as? [String: Int])?["id"], 2)
  }

  func testRejectsSnapshotFromAnotherApplication() {
    let other: [String: Any] = ["type": 4, "result": ["pid": 11, "id": 1]]
    XCTAssertNil(DTXSession.orderedTree([front, root, leaf], front: other))
  }

  func testIncompleteSnapshotFallsBackToLiveWalk() {
    XCTAssertNil(DTXSession.orderedTree([front, root], front: front))
  }

  func testPushCannotSatisfyAnAttributeRequestWithTheSameType() {
    let push: [String: Any] = ["type": 5, "notification": 0, "result": ["treeDump": [front, root, leaf]]]
    XCTAssertFalse(DTXSession.isReply(push, type: 5, object: nil))
  }

  func testAttributeReplyMustMatchRequestedObject() {
    XCTAssertTrue(DTXSession.isReply(leaf, type: 5, object: "10:2"))
    XCTAssertFalse(DTXSession.isReply(leaf, type: 5, object: "10:1"))
    XCTAssertFalse(DTXSession.isReply(leaf, type: nil, object: nil))
  }

  func testNotificationIsNotAReplyButRequestErrorsAre() {
    XCTAssertFalse(DTXSession.isReply(["type": 5, "notification": 1], type: 5, object: nil))
    XCTAssertTrue(DTXSession.isReply(["type": 5, "error": 8], type: 5, object: "10:2"))
  }

  func testScreenParsesCellsPlaceholdersSwitchesPushedRolesAndVisibility() throws {
    let responses: [[String: Any]] = [
      front,
      ["type": 5, "result": ["21": ["x": 0.0, "y": 0.0, "width": 402.0, "height": 874.0], "25": "com.apple.Preferences", "33": "Settings"]],
      // Table cell (75) maps to button; invisible (32:false) and blank text spacers are skipped.
      ["type": 5, "result": ["21": ["x": 0.0, "y": 100.0, "width": 400.0, "height": 44.0], "33": "General", "61": 75]],
      ["type": 5, "result": ["21": ["x": 0.0, "y": 130.0, "width": 400.0, "height": 20.0], "61": 48]],
      ["type": 5, "result": ["21": ["x": 0.0, "y": 150.0, "width": 400.0, "height": 44.0], "33": "Hidden Row", "61": 9, "32": false]],
      // Empty input falls back to Placeholder (42); switch suppresses 0/1 value in Label.
      ["type": 5, "result": ["21": ["x": 16.0, "y": 50.0, "width": 370.0, "height": 36.0], "42": "Search for food and places", "61": 45]],
      ["type": 5, "result": ["21": ["x": 300.0, "y": 200.0, "width": 50.0, "height": 30.0], "33": "Airplane Mode", "53": "1", "61": 40]],
      // First pushed tree lacks AutomationType (61) and uses AXRole (45).
      ["type": 5, "result": ["21": ["x": 20.0, "y": 800.0, "width": 80.0, "height": 40.0], "33": "Home", "53": "1", "45": 9]],
    ]
    let screen = try Screen(responses: responses, scale: 3, locked: false)
    XCTAssertEqual(screen.elements.map { $0["Label"] as? String },
                   ["General", "Search for food and places", "Airplane Mode", "Home"])
    XCTAssertEqual(screen.elements.map { $0["Role"] as? String },
                   ["button", "input", "switch", "tab"])
    XCTAssertEqual(screen.elements[2]["States"] as? [String], ["checked"])
    let pos = try screen.find("General", role: "button", index: nil)
    XCTAssertEqual(pos.0, 600, accuracy: 0.5)
    let search = try screen.find("Search for food", role: "input", index: nil)
    XCTAssertEqual(search.1, 204, accuracy: 0.5)
  }

  func testCanonicalDevicePathsAndAppMatchingNormalizePrivatePrefixAndBidiMarks() throws {
    XCTAssertEqual(Phone.canonicalDevicePath("file:///private/var/containers/Bundle/Application/A/Grab.app/"),
                   "/var/containers/Bundle/Application/A/Grab.app")
    XCTAssertEqual(Phone.canonicalDevicePath("/var/containers/Bundle/Application/A/Grab.app/Grab")
                     .flatMap { p in p.lastIndex(of: "/").map { String(p[..<$0]) } },
                   "/var/containers/Bundle/Application/A/Grab.app")
    let catalog: [[String: Any]] = [
      ["bundleIdentifier": "net.whatsapp.WhatsApp", "name": "\u{200e}WhatsApp"],
      ["bundleIdentifier": "com.grabtaxi.iphone", "name": "Grab"],
    ]
    XCTAssertEqual(try Phone.matchApp("whatsapp", in: catalog)?["bundleIdentifier"] as? String, "net.whatsapp.WhatsApp")
    XCTAssertEqual(try Phone.matchApp("COM.GRABTAXI.IPHONE", in: catalog)?["bundleIdentifier"] as? String, "com.grabtaxi.iphone")
    XCTAssertEqual(try Phone.validatedLink(" https://example.com/a "), "https://example.com/a")
    XCTAssertThrowsError(try Phone.validatedLink("https://"))
    XCTAssertThrowsError(try Phone.validatedLink("bad link"))
  }
}
