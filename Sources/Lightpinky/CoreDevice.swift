import Foundation
import AppleBridge

// CoreDevice ships no Swift module. These declarations describe only the ABI
// used on this Mac's Xcode build. Keep this small boundary separate from tools.
final class CoreObject {}
extension CoreObject {
  @_silgen_name("$s10CoreDevice0B7ManagerC7devicesSDy10Foundation4UUIDVAA06RemoteB0CGvgTj")
  func devices() -> [UUID: CoreObject]

  @_silgen_name("$s10CoreDevice06RemoteB0C07acquireB14UsageAssertion10withReason7options27invokingCompletionHandlerOn010completionL0So10NSProgressCSS_AA0eF7OptionsVSo17OS_dispatch_queueCSgys6ResultOyAA0beF0Cs5Error_pGctF")
  func acquire(_ reason: String, _ options: UnsafeRawPointer, _ queue: DispatchQueue?,
               _ completion: @escaping (Result<CoreObject, Error>) -> Void) -> Progress

  @_silgen_name("$s10CoreDevice06RemoteB0C06createC13XPCConnection14toServiceNamed14connectionMode16handlingEventsOn010completingM010completionySS_s6UInt64VSo17OS_dispatch_queueCAMSgys6ResultOySo0q12_xpc_remote_I0Cs5Error_pGctFTj")
  func connect(_ service: String, _ mode: UInt64, _ events: DispatchQueue, _ completionQueue: DispatchQueue?,
               _ completion: @escaping (Result<AnyObject, Error>) -> Void)

  @_silgen_name("$s10CoreDevice0B14UsageAssertionC27registerInvalidationHandler15invokingOnQueue7handlerySo17OS_dispatch_queueCSg_ys5Error_pSgctFTj")
  func onInvalidation(_ queue: DispatchQueue?, _ handler: @escaping (Error?) -> Void)
}

@_silgen_name("$s10CoreDevice0B7ManagerC6sharedACvgZ")
private func coreManager() -> CoreObject

// Resilient Swift structs return indirectly. Five words force an indirect
// result in this client; only the first two words are used by these APIs.
private struct CoreStorage {
  var a: UInt = 0, b: UInt = 0, c: UInt = 0, d: UInt = 0, e: UInt = 0
}
@_silgen_name("$s10CoreDevice21UsageAssertionOptionsV20requiredPreparednessAcA0bG0V_tcfC")
private func coreOptions(_ preparedness: UnsafeRawPointer) -> CoreStorage

struct PhoneError: LocalizedError {
  let message: String
  init(_ message: String) { self.message = message }
  var errorDescription: String? { message }
}

final class CoreConnection {
  // RemoteXPC reports disconnects on its callback queue. Input is never
  // replayed; the next tool call can create a fresh connection instead.
  private let stateLock = NSLock()
  private var disconnected = false
  var isValid: Bool {
    stateLock.lock(); defer { stateLock.unlock() }
    return !disconnected
  }
  private func invalidate() {
    stateLock.lock(); disconnected = true; stateLock.unlock()
  }
  let device: CoreObject
  private let identifier: String
  private let udid: String
  private var assertion: CoreObject?
  private let queue = DispatchQueue(label: "lightpinky.coredevice")
  private var channels: [String: AnyObject] = [:]
  private var directServices: [String:UInt16]?
  private var peerIP: String?
  private var directoryPort: UInt16?

  init(identifier: String, udid: String) throws {
    self.identifier = identifier
    self.udid = udid
    guard let id = UUID(uuidString: identifier) else { throw PhoneError("Invalid CoreDevice UUID") }
    let manager = coreManager()
    let deadline = ProcessInfo.processInfo.systemUptime + 8
    var found: CoreObject?
    repeat {
      found = manager.devices()[id]
      if found == nil { Thread.sleep(forTimeInterval: 0.1) }
    } while found == nil && ProcessInfo.processInfo.systemUptime < deadline
    guard let found else { throw PhoneError("CoreDevice has not discovered \(identifier)") }
    device = found
    // Developer mode (1) and DDI services (2). Neither extended hardware
    // information (4) nor a MobileDevice power assertion (8) is needed to
    // control these services. AMDevicePowerAssertionCreate can wedge over
    // Wi-Fi and invalidate an otherwise working tunnel.
    var preparedness: UInt = 3
    var options = withUnsafePointer(to: &preparedness) { coreOptions($0) }
    let semaphore = DispatchSemaphore(value: 0)
    var result: Result<CoreObject, Error>?
    let progress = withUnsafePointer(to: &options) {
      device.acquire("lightpinky phone control", $0, queue) { value in
        result = value
        semaphore.signal()
      }
    }
    guard semaphore.wait(timeout: .now() + 20) == .success else {
      progress.cancel()
      throw PhoneError("CoreDevice connection timed out")
    }
    assertion = try result!.get()
    // CoreDevice can replace the tunnel without delivering a RemoteXPC error
    // to every service channel. Its usage assertion is the lifetime authority.
    assertion!.onInvalidation(queue) { [weak self] _ in self?.invalidate() }
  }

  func channel(_ service: String) throws -> AnyObject {
    guard isValid else { throw PhoneError("CoreDevice connection ended; input was not retried") }
    if let cached = channels[service] { return cached }
    let semaphore = DispatchSemaphore(value: 0)
    var result: Result<AnyObject, Error>?
    device.connect(service, 0, queue, queue) { value in result = value; semaphore.signal() }
    guard semaphore.wait(timeout: .now() + 10) == .success else {
      invalidate()
      throw PhoneError("CoreDevice service \(service) timed out")
    }
    let connection = try result!.get()
    LPRemoteActivate(connection) { [weak self] event in
      if xpc_get_type(event) == XPC_TYPE_ERROR { self?.invalidate() }
    }
    channels[service] = connection
    return connection
  }

  func send(_ service: String, _ dictionary: [String: Any]) throws {
    LPRemoteSend(try channel(service), Self.xpc(dictionary))
  }

  func socket(_ service: String) throws -> FileHandle {
    guard isValid else { throw PhoneError("CoreDevice connection ended") }
    guard let port=try discoverServices()[service],let peerIP else {
      throw PhoneError("The phone did not advertise \(service)")
    }
    let fd=LPConnectTCP(peerIP,port)
    guard fd>=0 else { throw AccessibilityFailure.disconnected }
    return FileHandle(fileDescriptor:fd,closeOnDealloc:true)
  }

  func tunnelAddress() throws -> String {
    guard isValid else { throw PhoneError("CoreDevice connection ended") }
    if let peerIP { return peerIP }
    // Read macOS's local tunnel metadata. devicectl "details" also queries the
    // phone and can block startup even while its tunnel is already connected.
    func remote(_ arguments: [String]) throws -> String {
      String(decoding: try Command.run(arguments, timeout: 3,
        executable: "/usr/libexec/remotectl"), as: UTF8.self)
        .trimmingCharacters(in: .whitespacesAndNewlines)
    }
    var interface: String?
    for line in try remote(["list"]).split(separator: "\n") {
      let fields = line.split(whereSeparator: \.isWhitespace)
      guard fields.count > 1, fields[1].hasPrefix("coredevice-") else { continue }
      let id = String(fields[0])
      if try remote(["get-property", id, "UniqueDeviceID"]) == udid {
        interface = try remote(["get-property", id, "InterfaceName"])
        break
      }
    }
    guard let interface, !interface.isEmpty else {
      throw PhoneError("macOS has no connected tunnel for the selected phone")
    }
    let topology = String(decoding: try Command.run(
      ["-m","tcp","-p","remoted","-L","1","-n"], executable: "/usr/bin/nettop"), as: UTF8.self)
    for line in topology.split(separator: "\n") {
      let fields = line.split(separator: ",", omittingEmptySubsequences: false)
      guard fields.count > 3, fields[2] == interface, fields[3] == "Established",
            let range = fields[1].range(of: "<->") else { continue }
      let endpoint = fields[1][range.upperBound...]
      guard let dot = endpoint.lastIndex(of: "."),
            let port = UInt16(endpoint[endpoint.index(after: dot)...]), port > 0 else { continue }
      let peer = String(endpoint[..<dot])
      peerIP = peer
      directoryPort = port
      return peer
    }
    throw PhoneError("macOS has no active Remote Service Discovery connection to this phone")
  }

  func displayInfo() throws -> [String: Any] {
    let output = try invoke("com.apple.coredevice.deviceinfo",
      feature: "com.apple.coredevice.feature.getdisplayinfo", input: [:])
    guard let result = Self.object(output) as? [String: Any] else {
      throw PhoneError("Invalid native display information")
    }
    return result
  }

  func discoverServices() throws -> [String:UInt16] {
    if let directServices { return directServices }
    let peer = try tunnelAddress()
    guard let port = directoryPort else { throw PhoneError("Service-directory port unavailable") }
    let handshake=try ServiceDirectory(peer:peer,port:port).read()
    guard let services=handshake["Services"] as? [String:[String:Any]],
          let properties=handshake["Properties"] as? [String:Any],
          properties["UniqueDeviceID"] as? String == udid else {
      throw PhoneError("Service directory does not match the selected phone")
    }
    let ports=services.compactMapValues { value -> UInt16? in
      guard let text=value["Port"] as? String,let port=UInt16(text),port>0 else { return nil }
      return port
    }
    directServices=ports
    return ports
  }

  func invalidateDirectory() {
    directServices=nil
    peerIP=nil
    directoryPort=nil
  }

  func request(_ service: String, _ dictionary: [String: Any]) throws -> xpc_object_t {
    let semaphore = DispatchSemaphore(value: 0)
    var response: xpc_object_t?
    LPRemoteRequest(try channel(service), Self.xpc(dictionary), queue) {
      response = $0
      semaphore.signal()
    }
    guard semaphore.wait(timeout: .now() + 8) == .success, let response else {
      invalidate()
      throw PhoneError("CoreDevice service request timed out; input was not retried")
    }
    if xpc_get_type(response) == XPC_TYPE_ERROR {
      invalidate()
      throw PhoneError("CoreDevice service disconnected; input was not retried")
    }
    if xpc_dictionary_get_value(response, "error") != nil { throw PhoneError("CoreDevice rejected the service request") }
    return response
  }

  func invoke(_ service: String, feature: String, action: String = "", input: [String: Any]) throws -> xpc_object_t {
    var message: [String: Any] = [
      "CoreDevice.CoreDeviceDDIProtocolVersion": Int64(2),
      "CoreDevice.action": [String: Any](),
      "CoreDevice.coreDeviceVersion": ["components": [UInt64(651),UInt64(32)],
        "originalComponentsCount":Int64(2),"stringValue":"651.32"],
      "CoreDevice.deviceIdentifier": identifier,
      "CoreDevice.featureIdentifier": feature,
      "CoreDevice.input": input,
      "CoreDevice.invocationIdentifier": UUID().uuidString,
    ]
    if !action.isEmpty { message["CoreDevice.actionIdentifier"] = action }
    let response = try request(service,message)
    if let output = xpc_dictionary_get_value(response,"CoreDevice.output") { return output }
    if let error = xpc_dictionary_get_value(response,"CoreDevice.error"),
       let info = xpc_dictionary_get_value(error,"userInfo"),
       let description = xpc_dictionary_get_string(info,"NSLocalizedDescription") {
      throw PhoneError("\(String(cString:description)) (CoreDevice \(xpc_dictionary_get_int64(error,"code")))")
    }
    throw PhoneError("CoreDevice invocation failed")
  }

  static func xpc(_ value: Any) -> xpc_object_t {
    switch value {
    case let dictionary as [String: Any]:
      let object = xpc_dictionary_create(nil, nil, 0)
      for (key, value) in dictionary { xpc_dictionary_set_value(object, key, xpc(value)) }
      return object
    case let data as Data:
      return data.withUnsafeBytes { xpc_data_create($0.baseAddress, data.count) }
    case let value as UUID:
      var bytes = value.uuid
      return withUnsafeBytes(of:&bytes) { xpc_uuid_create($0.baseAddress!.assumingMemoryBound(to:UInt8.self)) }
    case let value as String: return xpc_string_create(value)
    case let value as UInt64: return xpc_uint64_create(value)
    case let value as Int64: return xpc_int64_create(value)
    case let value as Bool: return xpc_bool_create(value)
    case is NSNull: return xpc_null_create()
    case let values as [Any]:
      let object = xpc_array_create(nil, 0)
      for value in values { xpc_array_append_value(object, xpc(value)) }
      return object
    default: preconditionFailure("Unsupported XPC value \(type(of: value))")
    }
  }

  static func object(_ value: xpc_object_t) -> Any {
    let type = xpc_get_type(value)
    if type == XPC_TYPE_DICTIONARY {
      var result: [String: Any] = [:]
      xpc_dictionary_apply(value) { key, child in
        result[String(cString: key)] = object(child)
        return true
      }
      return result
    }
    if type == XPC_TYPE_ARRAY {
      return (0..<xpc_array_get_count(value)).map { object(xpc_array_get_value(value, $0)) }
    }
    if type == XPC_TYPE_STRING, let string = xpc_string_get_string_ptr(value) { return String(cString: string) }
    if type == XPC_TYPE_BOOL { return xpc_bool_get_value(value) }
    if type == XPC_TYPE_INT64 { return xpc_int64_get_value(value) }
    if type == XPC_TYPE_UINT64 { return xpc_uint64_get_value(value) }
    if type == XPC_TYPE_DOUBLE { return xpc_double_get_value(value) }
    return NSNull()
  }

  deinit {
    for connection in channels.values { LPRemoteCancel(connection) }
  }
}
