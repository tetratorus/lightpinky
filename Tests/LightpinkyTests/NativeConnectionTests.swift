import XCTest
import Darwin
@testable import LightpinkyCore

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
