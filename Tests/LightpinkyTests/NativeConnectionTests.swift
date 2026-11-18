import XCTest
import Darwin
@testable import LightpinkyCore

final class LockStateTests: XCTestCase {
  private func frame(_ value: [String: Any]) throws -> Data {
    let body = try PropertyListSerialization.data(fromPropertyList: value, format: .binary, options: 0)
    var size = UInt32(body.count).bigEndian
    return withUnsafeBytes(of: &size) { Data($0) } + body
  }

  private func withPeer(_ replies: Data, _ body: (FileHandle, FileHandle) throws -> Void) throws {
    var descriptors = [Int32](repeating: 0, count: 2)
    guard socketpair(AF_UNIX, SOCK_STREAM, 0, &descriptors) == 0 else {
      throw PhoneError("Cannot create test socket")
    }
    let client = FileHandle(fileDescriptor: descriptors[0], closeOnDealloc: true)
    let peer = FileHandle(fileDescriptor: descriptors[1], closeOnDealloc: true)
    defer { try? client.close(); try? peer.close() }
    try peer.write(contentsOf: replies)
    try body(client, peer)
  }

  private func checkin() throws -> Data {
    try frame(["Request": "RSDCheckin"]) + frame(["Request": "StartService"])
  }

  func testReadsChangingLockStateOnOneConnection() throws {
    let replies = try checkin() + frame(["Value": true]) + frame(["Value": false])
    try withPeer(replies) { client, _ in
      let reader = try LockStateReader(socket: client)
      XCTAssertTrue(try reader.locked())
      XCTAssertFalse(try reader.locked())
    }
  }

  func testMissingOrNonBooleanStateNeverMeansUnlocked() throws {
    for value: [String: Any] in [[:], ["Value": 0], ["Value": 1], ["Value": "false"]] {
      try withPeer(checkin() + frame(value)) { client, _ in
        let reader = try LockStateReader(socket: client)
        XCTAssertThrowsError(try reader.locked()) { error in
          XCTAssertFalse(error is LockStateTransportFailure)
        }
      }
    }
  }

  func testRejectsOversizedFrameBeforeReadingPayload() throws {
    try withPeer(checkin() + Data([0, 0x10, 0, 1])) { client, _ in
      let reader = try LockStateReader(socket: client)
      XCTAssertThrowsError(try reader.locked()) { error in
        XCTAssertTrue(error.localizedDescription.contains("frame size"))
      }
    }
  }

  func testTruncatedResponseFailsInsteadOfReturningOldState() throws {
    try withPeer(checkin() + frame(["Value": true]) + Data([0, 0])) { client, peer in
      let reader = try LockStateReader(socket: client)
      XCTAssertTrue(try reader.locked())
      shutdown(peer.fileDescriptor, SHUT_WR)
      XCTAssertThrowsError(try reader.locked()) { error in
        XCTAssertTrue(error is LockStateTransportFailure)
      }
    }
  }

  func testCheckinErrorIsNotAccepted() throws {
    try withPeer(frame(["Error": "PairingRequired"])) { client, _ in
      XCTAssertThrowsError(try LockStateReader(socket: client)) { error in
        XCTAssertTrue(error.localizedDescription.contains("PairingRequired"))
      }
    }
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
}
