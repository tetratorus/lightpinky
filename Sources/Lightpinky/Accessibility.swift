import Foundation
import AppleBridge

enum AccessibilityFailure: LocalizedError {
  case disconnected
  case timeout(String)
  var errorDescription:String? {
    switch self {
    case .disconnected: return "Accessibility connection closed"
    case .timeout(let stage): return "Accessibility \(stage) timed out"
    }
  }
}

/// DTX over the socket advertised by the existing CoreDevice tunnel.
/// Framing follows go-ios (MIT); AX requests follow this repository's former
/// driver. Foundation handles keyed archives; macOS owns pairing and routing.
final class AccessibilitySession {
  private let socket: FileHandle
  private let condition = NSCondition()
  private let writes = NSLock()
  private var sequence: UInt32 = 5
  private var closed = false
  private var failure: Error?
  private var waitingID: UInt32?
  private var rpcResult: Any?
  private var waitingType: Int?
  private var axResult: [String:Any]?
  private let limit = 20 * 1024 * 1024

  init(socket: FileHandle) throws {
    self.socket=socket
    var timeout=timeval(tv_sec:5,tv_usec:0)
    setsockopt(socket.fileDescriptor,SOL_SOCKET,SO_SNDTIMEO,&timeout,socklen_t(MemoryLayout.size(ofValue:timeout)))
    DispatchQueue(label:"lightpinky.accessibility.receive").async { [weak self] in self?.readLoop() }
    do {
      // Uncompressed DTX avoids a second codec and preserves complete messages.
      _ = try call("_notifyOfPublishedCapabilities:",args:[
        ["com.apple.private.DTXBlockCompression":0,"com.apple.private.DTXConnection":1]
      ])
      let identifier = nextID()
      condition.lock(); waitingID=identifier; rpcResult=nil; condition.unlock()
      try send(id:identifier,reply:true,value:"deviceAPIVersion")
      let version = try wait(seconds:5,ax:false)
      guard (version as? NSNumber)?.intValue ?? 0 > 0 else { throw PhoneError("Invalid accessibility API version") }
      _ = try call("clientNeedsAccessibility:",args:[true])
    } catch {
      close()
      throw error
    }
  }

  func close() {
    condition.lock()
    let wasClosed=closed
    closed=true
    condition.broadcast()
    condition.unlock()
    if wasClosed { return }
    _ = try? call("clientNeedsAccessibility:",args:[false])
    // Closing the socket also releases the phone's accessibility client.
    shutdown(socket.fileDescriptor,SHUT_RDWR)
    // Let the receive loop finish before FileHandle closes the descriptor,
    // so another connection cannot reuse it while that loop is unwinding.
  }

  private func nextID() -> UInt32 {
    condition.lock(); defer { condition.unlock() }
    let id=sequence; sequence &+= 1; return id
  }
  private static func append(_ value: UInt64, bytes: Int = 4, to data: inout Data) {
    for i in 0..<bytes { data.append(UInt8(truncatingIfNeeded:value >> (8*i))) }
  }
  private func send(id: UInt32, conversation: UInt32 = 0, channel: UInt32 = 0,
                    reply: Bool = false, kind: UInt32 = 2, value: Any? = nil, args: [Any] = []) throws {
    var auxiliary=Data()
    for arg in args {
      let archive=try NSKeyedArchiver.archivedData(withRootObject:arg,requiringSecureCoding:true)
      Self.append(10,to:&auxiliary); Self.append(2,to:&auxiliary)
      Self.append(UInt64(archive.count),to:&auxiliary); auxiliary.append(archive)
    }
    if !auxiliary.isEmpty {
      var header=Data()
      Self.append(496,bytes:8,to:&header); Self.append(UInt64(auxiliary.count),bytes:8,to:&header)
      auxiliary=header+auxiliary
    }
    let payload=try value.map { try NSKeyedArchiver.archivedData(withRootObject:$0,requiringSecureCoding:true) } ?? Data()
    var body=Data()
    Self.append(UInt64(kind),to:&body); Self.append(UInt64(auxiliary.count),to:&body)
    Self.append(UInt64(auxiliary.count+payload.count),to:&body); Self.append(0,to:&body)
    body.append(auxiliary); body.append(payload)
    var data=Data()
    for number in [UInt32(0x1f3d5b79),32,0x10000,UInt32(body.count),id,conversation,channel,reply ? 1 : 0] {
      Self.append(UInt64(number),to:&data)
    }
    data.append(body)
    writes.lock(); defer { writes.unlock() }
    try data.withUnsafeBytes { bytes in
      var offset=0
      while offset<bytes.count {
        let count=Darwin.send(socket.fileDescriptor,bytes.baseAddress!.advanced(by:offset),bytes.count-offset,0)
        if count<0 && errno==EINTR { continue }
        guard count>0 else { throw AccessibilityFailure.disconnected }
        offset+=count
      }
    }
  }
  @discardableResult private func call(_ selector: String,args: [Any] = []) throws -> UInt32 {
    let id=nextID()
    try send(id:id,value:selector,args:args)
    return id
  }
  private func exact(_ count: Int) throws -> Data {
    guard count>=0 && count<=limit else { throw PhoneError("Oversized accessibility message") }
    var data=Data(count:count)
    try data.withUnsafeMutableBytes { bytes in
      var offset=0
      while offset<count {
        let n=recv(socket.fileDescriptor,bytes.baseAddress!.advanced(by:offset),count-offset,0)
        if n<0 && errno==EINTR { continue }
        guard n>0 else { throw AccessibilityFailure.disconnected }
        offset+=n
      }
    }
    return data
  }
  private static func uint(_ data: Data,_ offset: Int,_ count: Int = 4) throws -> UInt32 {
    guard offset>=0, offset+count<=data.count else { throw PhoneError("Truncated accessibility message") }
    return data.subdata(in:offset..<(offset+count)).enumerated().reduce(UInt32(0)) {
      $0 | UInt32($1.element) << (8*$1.offset)
    }
  }
  private static func unarchive(_ data: Data) throws -> Any {
    try NSKeyedUnarchiver.unarchivedObject(ofClasses:[
      NSString.self,NSNumber.self,NSData.self,NSArray.self,NSDictionary.self,
      NSNull.self,NSError.self,NSUUID.self,NSDate.self,NSValue.self,NSURL.self
    ],from:data) ?? NSNull()
  }

  private func readLoop() {
    var fragments: [String:Data]=[:]
    do {
      while true {
        let header=try exact(32)
        guard try Self.uint(header,0)==0x1f3d5b79, try Self.uint(header,4)==32 else { throw PhoneError("Invalid DTX header") }
        let index=try Self.uint(header,8,2), count=try Self.uint(header,10,2)
        let length=Int(try Self.uint(header,12)), id=try Self.uint(header,16)
        let conversation=try Self.uint(header,20), channel=try Self.uint(header,24)
        let reply=try Self.uint(header,28) != 0
        let key="\(id):\(conversation):\(channel)"
        guard count>0, index<count, length<=limit else { throw PhoneError("Invalid DTX fragment") }
        if count>1 && index==0 {
          guard fragments.count<8 else { throw PhoneError("Too many fragmented accessibility messages") }
          fragments[key]=Data()
          continue
        }
        var body=try exact(length)
        if count>1 {
          guard let previous=fragments[key], previous.count+body.count<=limit else { throw PhoneError("Invalid DTX fragment sequence") }
          body=previous+body
          if index<count-1 { fragments[key]=body; continue }
          fragments.removeValue(forKey:key)
        }
        let kind=try Self.uint(body,0)
        let auxiliary=Int(try Self.uint(body,4)), total=Int(try Self.uint(body,8))
        guard auxiliary<=total, total<=body.count-16 else { throw PhoneError("Invalid DTX payload length") }
        guard kind != 0x707 else { throw PhoneError("Device sent compression that was not negotiated") }
        let value:Any
        do {
          value = total>auxiliary ? try Self.unarchive(body.subdata(in:(16+auxiliary)..<(16+total))) : NSNull()
        } catch {
          throw PhoneError("DTX payload decode failed (type \(kind), \(total-auxiliary) bytes): \(error.localizedDescription)")
        }
        var args:[Any]=[]
        if auxiliary>0 {
          guard auxiliary>=16 else { throw PhoneError("Invalid DTX arguments") }
          var offset=32
          while offset<16+auxiliary {
            guard try Self.uint(body,offset)==10, try Self.uint(body,offset+4)==2 else { throw PhoneError("Unsupported DTX argument") }
            let size=Int(try Self.uint(body,offset+8)); offset+=12
            guard size<=16+auxiliary-offset else { throw PhoneError("Truncated DTX argument") }
            do { args.append(try Self.unarchive(body.subdata(in:offset..<(offset+size)))) }
            catch { throw PhoneError("DTX argument decode failed (type \(kind), \(size) bytes): \(error.localizedDescription)") }
            offset+=size
          }
        }
        if value as? String == "hostAPIVersion" {
          try send(id:id,conversation:conversation+1,channel:channel,kind:3,value:7)
        } else if reply {
          try send(id:id,conversation:conversation+1,channel:channel,kind:0)
        }
        if value as? String == "processDataFromRemoteDevice:", let data=args.first as? Data {
          var error:NSError?
          guard let result=LPAXDecode(data,&error) as? [String:Any] else {
            throw PhoneError("AX response decode failed: \(error?.debugDescription ?? "invalid archive")")
          }
          condition.lock()
          if result["type"] as? Int == waitingType, (result["notification"] as? Int ?? 0)==0 {
            axResult=result
            condition.broadcast()
          }
          condition.unlock()
        } else if conversation>0 {
          condition.lock()
          if waitingID==id {
            if kind==4 { failure=PhoneError("Accessibility service rejected its handshake: \(value)") }
            else { rpcResult=value }
            condition.broadcast()
          }
          condition.unlock()
        }
      }
    } catch {
      condition.lock()
      if !closed { failure=error }
      condition.broadcast()
      condition.unlock()
    }
  }
  private func wait(seconds: Double,ax: Bool) throws -> Any {
    let deadline=Date(timeIntervalSinceNow:seconds)
    condition.lock(); defer { condition.unlock() }
    while !closed && failure==nil && (ax ? axResult==nil : rpcResult==nil) {
      if !condition.wait(until:deadline) { break }
    }
    if let failure { throw failure }
    guard !closed else { throw AccessibilityFailure.disconnected }
    guard let result:Any = ax ? axResult : rpcResult else {
      throw AccessibilityFailure.timeout(ax ? "request \(waitingType ?? -1)" : "API exchange")
    }
    return result
  }
  private func request(_ type: Int,object:[String:Any]? = nil,parameters:[String:Any]? = nil) throws -> [String:Any] {
    var error:NSError?
    guard let data=LPAXRequestData(UInt(type),object,parameters,&error) else { throw error ?? PhoneError("Cannot encode accessibility request") as NSError }
    condition.lock(); waitingType=type; axResult=nil; condition.unlock()
    try call("processDataFromHost:",args:[data])
    let response=try wait(seconds:3,ax:true) as! [String:Any]
    guard (response["error"] as? Int ?? 0)==0 else { throw PhoneError("Accessibility returned error \(response["error"]!)") }
    return response
  }

  func tree() throws -> [[String:Any]] {
    let front=try request(4)
    guard let app=front["result"] as? [String:Any], app["id"] != nil else { throw PhoneError("No foreground accessibility application") }
    var responses=[front], stack:[([String:Any],Int)]=[(app,0)], seen=Set<String>()
    let deadline=ProcessInfo.processInfo.systemUptime+8
    let attributes=[8,9,21,33,53,26,25,61,77,27,30,28,42,64,32,66,69,45]
    while let (object,depth)=stack.popLast() {
      if responses.count>400 || ProcessInfo.processInfo.systemUptime>=deadline {
        if responses.count>=2 { break }
        throw AccessibilityFailure.timeout("screen walk")
      }
      let key="\(object["pid"] ?? ""):\(object["id"] ?? "")"
      if depth>40 || !seen.insert(key).inserted { continue }
      let response=try request(5,object:object,parameters:["attributes":attributes])
      responses.append(response)
      if let data=response["result"] as? [String:Any] {
        let children=(data["9"] as? [[String:Any]]) ?? (data["8"] as? [[String:Any]]) ?? []
        for child in children.reversed() { stack.append((child,depth+1)) }
      }
    }
    return responses
  }
}
