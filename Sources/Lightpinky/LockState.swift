import Foundation
import Darwin

/// Read-only lockdown requests over the existing authenticated CoreDevice
/// tunnel. No pairing, TLS session, heartbeat or second tunnel is created.
final class LockStateReader {
  private let socket: FileHandle

  convenience init(connection: CoreConnection) throws {
    try self.init(socket: connection.socket("com.apple.mobile.lockdown.remote.trusted"))
  }

  init(socket: FileHandle) throws {
    self.socket = socket
    do {
      try send(["Request": "RSDCheckin", "Label": "lightpinky", "ProtocolVersion": "2"])
      let deadline = ProcessInfo.processInfo.systemUptime + 5
      let checkin = try receive(deadline: deadline)
      let service = try receive(deadline: deadline)
      guard checkin["Request"] as? String == "RSDCheckin",
            service["Request"] as? String == "StartService" else {
        throw PhoneError("Unexpected lockdown service check-in")
      }
    } catch {
      try? socket.close()
      throw error
    }
  }

  deinit { try? socket.close() }

  func locked() throws -> Bool {
    try send(["Request": "GetValue", "Label": "lightpinky", "Key": "PasswordProtected"])
    let result = try receive(deadline: ProcessInfo.processInfo.systemUptime + 3)
    guard let value = result["Value"] as? NSNumber,
          CFGetTypeID(value) == CFBooleanGetTypeID() else {
      throw PhoneError("Lockdown did not return a boolean lock state")
    }
    return value.boolValue
  }

  private func send(_ request: [String: Any]) throws {
    let body = try PropertyListSerialization.data(fromPropertyList: request, format: .xml, options: 0)
    var size = UInt32(body.count).bigEndian
    var data = withUnsafeBytes(of: &size) { Data($0) }
    data.append(body)
    var timeout = timeval(tv_sec: 3, tv_usec: 0)
    setsockopt(socket.fileDescriptor, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
    try socket.write(contentsOf: data)
  }

  private func read(_ count: Int, deadline: Double) throws -> Data {
    var data = Data()
    while data.count < count {
      let remaining = deadline - ProcessInfo.processInfo.systemUptime
      guard remaining > 0 else { throw PhoneError("Lock-state read timed out") }
      var ready = pollfd(fd: socket.fileDescriptor, events: Int16(POLLIN), revents: 0)
      let result = poll(&ready, 1, Int32(remaining * 1000))
      if result < 0 && errno == EINTR { continue }
      guard result > 0 else { throw PhoneError("Lock-state read timed out") }
      var chunk = Data(count: min(count - data.count, 16384))
      let received = chunk.withUnsafeMutableBytes {
        Darwin.recv(socket.fileDescriptor, $0.baseAddress, $0.count, 0)
      }
      if received < 0 && errno == EINTR { continue }
      guard received > 0 else { throw PhoneError("Lock-state service disconnected") }
      chunk.count = received
      data.append(chunk)
    }
    return data
  }

  private func receive(deadline: Double) throws -> [String: Any] {
    let header = try read(4, deadline: deadline)
    let size = header.reduce(UInt32(0)) { ($0 << 8) | UInt32($1) }
    guard size > 0, size <= 1_048_576 else { throw PhoneError("Invalid lockdown frame size") }
    let body = try read(Int(size), deadline: deadline)
    guard let result = try PropertyListSerialization.propertyList(from: body, format: nil) as? [String: Any] else {
      throw PhoneError("Invalid lockdown response")
    }
    if let error = result["Error"] as? String { throw PhoneError("Lock-state service: \(error)") }
    return result
  }
}
