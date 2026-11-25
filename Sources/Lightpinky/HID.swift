import Foundation
import AppleBridge

final class HID {
  private let connection: CoreConnection
  private var keyboard: UInt64?
  private var media: Media?
  private var lastStreamUse: Double?
  // Connections can outlive an HID wrapper (notably across unlock attempts).
  // Keep report timestamps monotonic when that wrapper is recreated.
  private static let epoch = ProcessInfo.processInfo.systemUptime
  private let universal = "com.apple.coredevice.hid.universalhidservice"
  private let indigo = "com.apple.coredevice.hid.indigo"

  private func ensureStream() throws -> Media {
    if media == nil { media = try Media(connection:connection) }
    lastStreamUse = ProcessInfo.processInfo.systemUptime
    return media!
  }

  /// Starts the display session if needed and returns its video decoder.
  func video() throws -> VideoDecoder {
    try ensureStream().video
  }

  func endDisplaySession() {
    media?.stopAll()
    media = nil
    lastStreamUse = nil
  }

  func endIdleDisplaySession(after seconds: Double, now: Double = ProcessInfo.processInfo.systemUptime) {
    guard let last = lastStreamUse, now - last >= seconds else { return }
    endDisplaySession()
  }

  init(connection: CoreConnection) {
    self.connection = connection
  }

  func button(page: UInt64 = 12, usage: UInt64) throws {
    func send(_ state: UInt64) throws {
      try connection.send(indigo, ["messageType": "IndigoButtonEvent",
        "featureIdentifier": "com.apple.coredevice.feature.remote.hid.button",
        "payload": ["state": state, "usagePage": page, "usageCode": usage]])
    }
    try send(1)
    Thread.sleep(forTimeInterval: 0.06)
    try send(2)
  }

  private func report(_ data: Data, surface: UInt64) throws {
    try connection.send(universal, [
      "featureIdentifier": "com.apple.coredevice.feature.remote.universalhidservice",
      "messageType": "Request", "payload": ["send": ["_0": data, "_1": surface]],
    ])
  }

  private func stamp(_ data: inout Data, at offset: Int) {
    let epoch = Self.epoch
    let ticks = UInt64((ProcessInfo.processInfo.systemUptime - epoch) * 24_000_000)
    for i in 0..<6 { data[offset + i] = UInt8(truncatingIfNeeded: ticks >> (8 * i)) }
  }

  func contact(_ point: CGPoint, down: Bool) throws {
    _ = try ensureStream()
    let x = UInt16((min(1, max(0, point.x)) * 65535).rounded())
    let y = UInt16((min(1, max(0, point.y)) * 65535).rounded())
    var data = Data(repeating: 0, count: 58)
    data[0] = 9; data[1] = 1; data[2] = 5; data[3] = down ? 0xc2 : 2
    data[4] = UInt8(truncatingIfNeeded: x); data[5] = UInt8(x >> 8)
    data[6] = UInt8(truncatingIfNeeded: y); data[7] = UInt8(y >> 8)
    data[40] = 2
    stamp(&data, at: 44)
    try report(data, surface: 257)
  }

  /// Points are fractions in the physical panel's upright orientation.
  func gesture(from: CGPoint, to: CGPoint, duration: Double) throws {
    try contact(from, down: true)
    defer { try? contact(to, down: false) }
    let steps = max(1, Int(duration * 60))
    for i in 1...steps {
      Thread.sleep(forTimeInterval: duration / Double(steps))
      let t = Double(i) / Double(steps)
      try contact(CGPoint(x: from.x + (to.x - from.x) * t,
                          y: from.y + (to.y - from.y) * t), down: true)
    }
  }

  func prepareKeyboard() throws {
    _ = try ensureStream()
    if keyboard != nil { return }
    let id: UInt64 = 0x100002000 + UInt64.random(in: 1...0xfffff)
    let descriptor = Data([
      0x05,0x01,0x09,0x06,0xa1,0x01,0x05,0x07,0x19,0xe0,0x29,0xe7,
      0x15,0x00,0x25,0x01,0x95,0x08,0x75,0x01,0x81,0x02,
      0x95,0x01,0x75,0x08,0x81,0x01,0x05,0x07,0x19,0x00,0x29,0xff,
      0x15,0x00,0x26,0xff,0x00,0x95,0x06,0x75,0x08,0x81,0x00,
      0x05,0x08,0x19,0x01,0x29,0x05,0x15,0x00,0x25,0x01,0x95,0x05,
      0x75,0x01,0x91,0x02,0x95,0x01,0x75,0x03,0x91,0x01,0xc0,
    ])
    let storage: [String: Any] = [
      "Manufacturer": ["string": "lightpinky"], "Product": ["string": "lightpinky keyboard"],
      "ProductID": ["int": Int64(0x250)], "VendorID": ["int": Int64(0x5ac)],
      "PrimaryUsage": ["int": Int64(6)], "PrimaryUsagePage": ["int": Int64(1)],
      "DeviceUsagePairs": ["array": [["dictionary": [
        "DeviceUsage": ["int": Int64(6)], "DeviceUsagePage": ["int": Int64(1)]]]]],
      "Transport": ["string": "USB"], "ReportDescriptor": ["data": descriptor],
      "UniversalControlVirtualService": ["bool": true], "_ServiceID": ["uint": id],
    ]
    let response = try connection.request(universal, [
      "featureIdentifier": "com.apple.coredevice.feature.remote.universalhidservice",
      "messageType": "Request", "payload": ["createService": ["_0": [
        "DeviceUsagePairs": [["DeviceUsage": Int64(6), "DeviceUsagePage": Int64(1)]],
        "PrimaryUsage": UInt64(6), "PrimaryUsagePage": UInt64(1),
        "Product": "lightpinky keyboard", "ProductID": Int64(0x250), "VendorID": Int64(0x5ac),
        "_CoreDevice_codablePropertyStorage": storage, "_ServiceID": id,
      ]]],
    ])
    let assigned = xpc_dictionary_get_uint64(response, "serviceID")
    keyboard = assigned == 0 ? id : assigned
    Thread.sleep(forTimeInterval: 0.3)
  }

  func keys(_ usages: [UInt8]) throws {
    try prepareKeyboard()
    func send(_ keys: [UInt8]) throws {
      var data = Data(repeating: 0, count: 39)
      data[0] = 1
      for key in keys where key < 240 { data[1 + Int(key) / 8] |= 1 << (key % 8) }
      stamp(&data, at: 31)
      try report(data, surface: keyboard!)
    }
    defer { try? send([]) }
    // iOS processes the bitmap's lower usages first. Establish modifiers
    // before the character, otherwise Command-V can insert a literal "v".
    let modifiers=usages.filter { (0xe0...0xe7).contains($0) }
    if !modifiers.isEmpty {
      try send(modifiers)
      Thread.sleep(forTimeInterval:0.015)
    }
    try send(usages)
    Thread.sleep(forTimeInterval: 0.035)
  }
}
