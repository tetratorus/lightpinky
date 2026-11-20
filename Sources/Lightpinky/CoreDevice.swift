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

  @_silgen_name("$s10CoreDevice06RemoteB0C16currentLockStateAA0eF0VvgTj")
  fileprivate func currentLockState() async throws -> CoreStorage

  @_silgen_name("$s10CoreDevice0B14UsageAssertionC27registerInvalidationHandler15invokingOnQueue7handlerySo17OS_dispatch_queueCSg_ys5Error_pSgctFTj")
  func onInvalidation(_ queue: DispatchQueue?, _ handler: @escaping (Error?) -> Void)
}

@_silgen_name("$s10CoreDevice0B7ManagerC6sharedACvgZ")
private func coreManager() -> CoreObject

// Resilient Swift structs return indirectly. Five words force an indirect
// result in this client; only the first two words are used by these APIs.
private struct CoreStorage {
  var a: UInt = 0, b: UInt = 0, c: UInt = 0, d: UInt = 0, e: UInt = 0

  @_silgen_name("$s10CoreDevice9LockStateV18isPasscodeRequiredSbvg")
  func isPasscodeRequired() -> Bool
}
@_silgen_name("$s10CoreDevice21UsageAssertionOptionsV20requiredPreparednessAcA0bG0V_tcfC")
private func coreOptions(_ preparedness: UnsafeRawPointer) -> CoreStorage

public struct PhoneError: LocalizedError {
  let message: String
  public init(_ message: String) { self.message = message }
  public var errorDescription: String? { message }
}

/// Only transport failures qualify for an observation retry. Device refusals,
/// malformed results and input operations must propagate without being replayed.
enum CoreConnectionFailure: LocalizedError {
  case timedOut(String)
  case disconnected(String)

  var errorDescription: String? {
    switch self {
    case .timedOut(let operation): return "CoreDevice \(operation) timed out"
    case .disconnected(let operation): return "CoreDevice \(operation) disconnected"
    }
  }

  static func retryRead<T>(_ read: () throws -> T) throws -> T {
    do { return try read() }
    catch is CoreConnectionFailure { return try read() }
  }
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
  let udid: String
  private var assertion: CoreObject?
  private let queue = DispatchQueue(label: "lightpinky.coredevice")
  private var channels: [String: AnyObject] = [:]
  private var directServices: [String:UInt16]?
  private var peerIP: String?
  private var directoryPort: UInt16?
  private let pasteboardNonce = "lightpinky-\(UUID().uuidString)"
  private var pasteboardSequence: Int64 = 0

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
    guard isValid else { throw CoreConnectionFailure.disconnected("connection") }
    if let cached = channels[service] { return cached }
    let semaphore = DispatchSemaphore(value: 0)
    var result: Result<AnyObject, Error>?
    device.connect(service, 0, queue, queue) { value in result = value; semaphore.signal() }
    guard semaphore.wait(timeout: .now() + 10) == .success else {
      invalidate()
      throw CoreConnectionFailure.timedOut("service \(service)")
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

  /// Service sockets go straight to the port the phone advertised. The port
  /// table is kept on disk per tunnel address, because every read of the
  /// phone's service directory displaces macOS's own directory connection.
  func socket(_ service: String) throws -> FileHandle {
    guard isValid else { throw PhoneError("CoreDevice connection ended") }
    for attempt in 0..<2 {
      let ports=try discoverServices()
      let peer=try peerIP ?? tunnelAddress()
      if let port=ports[service] {
        let fd=LPConnectTCP(peer,port)
        if fd>=0 {
          directOpened.insert(service)
          return FileHandle(fileDescriptor:fd,closeOnDealloc:true)
        }
      } else if attempt==1 {
        throw PhoneError("The phone did not advertise \(service)")
      }
      invalidateDirectory()
    }
    throw AccessibilityFailure.disconnected
  }

  private var portCache: URL {
    FileManager.default.homeDirectoryForCurrentUser
      .appendingPathComponent(".cache/lightpinky/run/ports-\(udid).json")
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

  private var layoutCache: URL {
    portCache.deletingLastPathComponent().appendingPathComponent("layout-\(udid).json")
  }
  private let reference = "com.apple.coredevice.deviceinfo"
  private var derived = false
  private static var strikes = 0
  /// A port from the table answered as the service it was looked up for.
  func confirmDirectory() { Self.strikes = 0 }
  private var directOpened: Set<String> = []

  private func save(_ object: [String:Any], to url: URL) {
    try? FileManager.default.createDirectory(at:url.deletingLastPathComponent(),withIntermediateDirectories:true,
      attributes:[.posixPermissions:0o700])
    if let data=try? JSONSerialization.data(withJSONObject:object) {
      try? data.write(to:url,options:.atomic)
      chmod(url.path,0o600)
    }
  }

  /// The phone gives its services one contiguous block of ports, in the same
  /// order on every tunnel; only the block's start moves. With the order
  /// remembered from one directory read, the port of a channel this process
  /// already holds locates the whole block, and no further read is needed.
  private func derivePorts() -> [String:UInt16]? {
    guard let data=try? Data(contentsOf:layoutCache),
          let offsets=try? JSONSerialization.jsonObject(with:data) as? [String:Int],
          offsets[reference] == 0, (try? displayInfo()) != nil,
          let listing=try? Command.run(["-m","tcp","-p","\(getpid())","-L","1","-n"],timeout:5,
            executable:"/usr/bin/nettop") else { return nil }
    var seen: [String:Set<Int>] = [:]
    for line in String(decoding:listing,as:UTF8.self).split(separator:"\n") {
      let fields=line.split(separator:",",omittingEmptySubsequences:false)
      guard fields.count>3, fields[2].hasPrefix("utun"), fields[3]=="Established",
            let range=fields[1].range(of:"<->") else { continue }
      let endpoint=fields[1][range.upperBound...]
      guard let dot=endpoint.lastIndex(of:"."), let port=Int(endpoint[endpoint.index(after:dot)...]) else { continue }
      seen[String(endpoint[..<dot]),default:[]].insert(port)
    }
    let held=Set(channels.keys).union(directOpened).union([reference]).compactMap { offsets[$0] }
    for (peer,ports) in seen where peerIP == nil || peerIP == peer {
      // Every port this process holds on the tunnel must land on a service
      // it opened; a start that explains them all is the block's start.
      for port in ports { for offset in held {
        let start=port-offset
        guard ports.allSatisfy({ held.contains($0-start) }) else { continue }
        let table=offsets.compactMapValues { UInt16(exactly:start+$0) }
        guard table.count == offsets.count else { continue }
        peerIP=peer
        return table
      } }
    }
    return nil
  }

  func discoverServices() throws -> [String:UInt16] {
    if let directServices { return directServices }
    if let table=derivePorts() {
      directServices=table
      derived=true
      return table
    }
    let peer = try tunnelAddress()
    if let data=try? Data(contentsOf:portCache),
       let saved=try? JSONSerialization.jsonObject(with:data) as? [String:Any],
       saved["peer"] as? String == peer, let ports=saved["ports"] as? [String:Int] {
      let cached=ports.compactMapValues { UInt16(exactly:$0) }
      if !cached.isEmpty { directServices=cached; return cached }
    }
    guard let port = directoryPort else { throw PhoneError("Service-directory port unavailable") }
    // Last resort: this read displaces macOS's own directory connection,
    // and its heartbeat then fails and takes the tunnel down within minutes.
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
    derived=false
    save(["peer":peer,"ports":ports.mapValues { Int($0) }],to:portCache)
    if let origin=ports[reference] {
      save(ports.mapValues { Int($0)-Int(origin) },to:layoutCache)
    }
    return ports
  }

  func invalidateDirectory() {
    // A derived table that fails on a live connection means the remembered
    // order is wrong (an iOS update, say); forget it so the next lookup reads
    // the directory. A dead connection says nothing about the order.
    // One failure can be a tunnel that died a moment ago, so it takes two
    // in a row before paying for a directory read.
    if derived && isValid {
      Self.strikes += 1
      if Self.strikes >= 2 { try? FileManager.default.removeItem(at:layoutCache); Self.strikes = 0 }
    }
    derived=false
    directServices=nil
    peerIP=nil
    directoryPort=nil
    try? FileManager.default.removeItem(at:portCache)
  }

  func request(_ service: String, _ dictionary: [String: Any], timeout: Double = 15) throws -> xpc_object_t {
    let semaphore = DispatchSemaphore(value: 0)
    var response: xpc_object_t?
    LPRemoteRequest(try channel(service), Self.xpc(dictionary), queue) {
      response = $0
      semaphore.signal()
    }
    guard semaphore.wait(timeout: .now() + timeout) == .success, let response else {
      invalidate()
      throw CoreConnectionFailure.timedOut("service request")
    }
    if xpc_get_type(response) == XPC_TYPE_ERROR {
      invalidate()
      throw CoreConnectionFailure.disconnected("service request")
    }
    if xpc_dictionary_get_value(response, "error") != nil { throw PhoneError("CoreDevice rejected the service request") }
    return response
  }

  private func invocation(feature: String, action: String = "", input: [String: Any]) -> [String: Any] {
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
    return message
  }

  /// Current DDIs stream application catalogs; the old listapps request can
  /// wedge dtappserviced. Use an isolated channel for its side-channel pushes.
  func stream(_ service: String, feature: String, input: [String: Any]) throws -> [Any] {
    guard isValid else { throw CoreConnectionFailure.disconnected("connection") }
    let connected = DispatchSemaphore(value: 0)
    var result: Result<AnyObject, Error>?
    device.connect(service, 0, queue, queue) { value in result=value; connected.signal() }
    guard connected.wait(timeout:.now()+10) == .success else {
      invalidate()
      throw CoreConnectionFailure.timedOut("stream connection")
    }
    let connection = try result!.get()
    defer { LPRemoteCancel(connection) }
    let condition = NSCondition()
    var elements: [Any] = []
    var finished = false
    var failure: Error?
    func receive(_ event: xpc_object_t) {
      condition.lock(); defer { condition.broadcast(); condition.unlock() }
      guard !finished else { return }
      if xpc_get_type(event) == XPC_TYPE_ERROR {
        failure=CoreConnectionFailure.disconnected("stream"); finished=true; return
      }
      guard let body=Self.object(event) as? [String:Any] else { return }
      if let error=body["CoreDevice.error"] {
        failure=PhoneError("CoreDevice stream failed: \(error)"); finished=true; return
      }
      guard let status=body["CoreDevice.XPCMessageKey.sideChannelStatus"] as? [String:Any] else { return }
      if let error=status["receivedError"] {
        failure=PhoneError("CoreDevice stream failed: \(error)"); finished=true
      } else if status["finishStreaming"] != nil {
        finished=true
      } else if let push=status["pushing"] as? [String:Any], let batch=push["elements"] as? [Any] {
        guard elements.count+batch.count<=10000 else {
          failure=PhoneError("CoreDevice stream exceeded its element limit"); finished=true; return
        }
        elements.append(contentsOf:batch)
      } else {
        failure=PhoneError("Malformed CoreDevice stream status"); finished=true
      }
    }
    LPRemoteActivate(connection,receive)
    let message=invocation(feature:feature,input:[
      "actualInput":input, "streamProxy":["sideChannel":UUID()],
    ])
    LPRemoteRequest(connection,Self.xpc(message),queue,receive)
    let deadline=Date(timeIntervalSinceNow:15)
    condition.lock(); defer { condition.unlock() }
    while !finished {
      if !condition.wait(until:deadline), !finished {
        // Ignore late batches after canceling this isolated stream.
        finished = true
        invalidate()
        throw CoreConnectionFailure.timedOut("stream")
      }
    }
    if let failure {
      if failure is CoreConnectionFailure { invalidate() }
      throw failure
    }
    return elements
  }

  func invoke(_ service: String, feature: String, action: String = "", input: [String: Any],
              timeout: Double = 15) throws -> xpc_object_t {
    let response = try request(service,invocation(feature:feature,action:action,input:input),timeout:timeout)
    if let output = xpc_dictionary_get_value(response,"CoreDevice.output") { return output }
    if let error = xpc_dictionary_get_value(response,"CoreDevice.error"),
       let info = xpc_dictionary_get_value(error,"userInfo"),
       let description = xpc_dictionary_get_string(info,"NSLocalizedDescription") {
      throw PhoneError("\(String(cString:description)) (CoreDevice \(xpc_dictionary_get_int64(error,"code")))")
    }
    if let error=xpc_dictionary_get_value(response,"CoreDevice.error") {
      throw PhoneError("CoreDevice \(feature) failed: \(Self.object(error))")
    }
    throw PhoneError("CoreDevice \(feature) returned no output: \(Self.object(response))")
  }

  func locked() throws -> Bool {
    guard isValid else { throw CoreConnectionFailure.disconnected("connection") }
    let ready=DispatchSemaphore(value:0)
    var result:Result<Bool,Error>?
    let device = device
    let task = Task {
      do { result = .success(try await device.currentLockState().isPasscodeRequired()) }
      catch { result = .failure(error) }
      ready.signal()
    }
    guard ready.wait(timeout:.now()+20) == .success else {
      task.cancel()
      invalidate()
      throw CoreConnectionFailure.timedOut("lock state")
    }
    return try result!.get()
  }

  func screenshotData() throws -> Data {
    let output=try invoke("com.apple.coredevice.screencaptureservice",
      feature:"com.apple.coredevice.feature.capturescreenshot",
      action:"com.apple.coredevice.action.capturescreenshot",
      input:["displayUniqueID":NSNull(),"requestedFormat":"png"],
      // A full-resolution PNG is over a megabyte. On a slow Wi-Fi link,
      // abandoning it early only queues another one behind it.
      timeout:30)
    guard let image=xpc_dictionary_get_value(output,"image"),
          xpc_get_type(image)==XPC_TYPE_DATA,
          let bytes=xpc_data_get_bytes_ptr(image),xpc_data_get_length(image)>0 else {
      throw PhoneError("CoreDevice screenshot returned no image data")
    }
    return Data(bytes:bytes,count:xpc_data_get_length(image))
  }

  func setPasteboard(_ text: String) throws {
    pasteboardSequence += 1
    let utf8 = "public.utf8-plain-text"
    let response = try request("com.apple.coredevice.pasteboardservice", [
      "command": "SET",
      "pasteboardName": "general",
      "items": [[
        "types": [utf8],
        "data": [utf8: ["data": Data(text.utf8)]],
      ]],
      // Local-only metadata prevents Universal Clipboard from overwriting the
      // owner's Mac clipboard when typing on the phone.
      "sourceMetadata": [
        "nonce": pasteboardNonce,
        "pasteboardName": "lightpinky",
        "changeCount": pasteboardSequence,
      ],
    ], timeout: 10)
    guard let reply = xpc_dictionary_get_string(response, "command"),
          String(cString: reply) == "SET_REPLY" else {
      throw PhoneError("Unexpected pasteboard reply")
    }
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
