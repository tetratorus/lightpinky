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
  let device: CoreObject
  private let identifier: String
  private var assertion: CoreObject?
  private let queue = DispatchQueue(label: "lightpinky.coredevice")
  private var channels: [String: AnyObject] = [:]
  private var directServices: [String:UInt16]?
  private var peerIP: String?

  init(identifier: String) throws {
    self.identifier = identifier
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
    var preparedness: UInt = 15
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
  }

  func channel(_ service: String) throws -> AnyObject {
    if let cached = channels[service] { return cached }
    let semaphore = DispatchSemaphore(value: 0)
    var result: Result<AnyObject, Error>?
    device.connect(service, 0, queue, queue) { value in result = value; semaphore.signal() }
    guard semaphore.wait(timeout: .now() + 10) == .success else {
      throw PhoneError("CoreDevice service \(service) timed out")
    }
    let connection = try result!.get()
    LPRemoteActivate(connection) { _ in }
    channels[service] = connection
    return connection
  }

  func send(_ service: String, _ dictionary: [String: Any]) throws {
    LPRemoteSend(try channel(service), Self.xpc(dictionary))
  }

  func socket(_ service: String) throws -> FileHandle {
    guard let port=try discoverServices()[service],let peerIP else {
      throw PhoneError("The phone did not advertise \(service)")
    }
    let fd=LPConnectTCP(peerIP,port)
    guard fd>=0 else { throw AccessibilityFailure.disconnected }
    return FileHandle(fileDescriptor:fd,closeOnDealloc:true)
  }

  func discoverServices() throws -> [String:UInt16] {
    if let directServices { return directServices }
    let details = try Command.device(["device","info","details"],id:identifier)
    guard let transport = details["connectionProperties"] as? [String: Any],
          let peer = transport["tunnelIPAddress"] as? String else { throw PhoneError("CoreDevice tunnel address unavailable") }
    peerIP = peer
    let topology = String(decoding:try Command.run(["-m","tcp","-p","remoted","-L","1","-n"],
      executable:"/usr/bin/nettop"),as:UTF8.self)
    let remotePrefix = "<->\(peer)."
    let port: UInt16? = topology.split(separator:"\n").compactMap { line in
      guard let range = line.range(of:remotePrefix) else { return nil }
      return UInt16(line[range.upperBound...].prefix(while: \.isNumber))
    }.first
    guard let port else { throw PhoneError("macOS has no active Remote Service Discovery connection to this phone") }
    let handshake=try ServiceDirectory(peer:peer,port:port).read()
    guard let services=handshake["Services"] as? [String:[String:Any]],
          let properties=handshake["Properties"] as? [String:Any],
          let udid=properties["UniqueDeviceID"] as? String,
          try PhoneInfo.discover().contains(where: { $0.id==identifier && $0.udid==udid }) else {
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
  }

  func request(_ service: String, _ dictionary: [String: Any]) throws -> xpc_object_t {
    let semaphore = DispatchSemaphore(value: 0)
    var response: xpc_object_t?
    LPRemoteRequest(try channel(service), Self.xpc(dictionary), queue) {
      response = $0
      semaphore.signal()
    }
    guard semaphore.wait(timeout: .now() + 8) == .success, let response else {
      throw PhoneError("CoreDevice service request timed out; input was not retried")
    }
    if xpc_get_type(response) == XPC_TYPE_ERROR { throw PhoneError("CoreDevice service disconnected") }
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
    case let values as [Any]:
      let object = xpc_array_create(nil, 0)
      for value in values { xpc_array_append_value(object, xpc(value)) }
      return object
    default: preconditionFailure("Unsupported XPC value \(type(of: value))")
    }
  }

  deinit {
    for connection in channels.values { LPRemoteCancel(connection) }
  }
}
