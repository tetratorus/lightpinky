import Foundation
import AppleBridge

/// Read the directory over the already-paired CoreDevice tunnel. This is the
/// receive-only go-ios Handshake, not HandshakeAsHost: macOS owns the host
/// announcement and its service lifetimes. No pairing or tunnel is created here.
final class ServiceDirectory {
  private let socket:FileHandle
  private let deadline=ProcessInfo.processInfo.systemUptime+8
  private let limit=4*1024*1024
  private var streams:[UInt32:Data]=[1:Data(),3:Data()]
  private var receivedBytes:[UInt32:Int]=[0:0,1:0,3:0]
  private var stage="HTTP/2 settings"

  init(peer:String,port:UInt16) throws {
    let fd=LPConnectTCP(peer,port)
    guard fd>=0 else { throw AccessibilityFailure.disconnected }
    socket=FileHandle(fileDescriptor:fd,closeOnDealloc:true)
    var timeout=timeval(tv_sec:3,tv_usec:0)
    setsockopt(fd,SOL_SOCKET,SO_SNDTIMEO,&timeout,socklen_t(MemoryLayout.size(ofValue:timeout)))
  }

  func read() throws -> [String:Any] {
    defer { try? socket.close() }
    try send(Data("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n".utf8))
    // SETTINGS_MAX_CONCURRENT_STREAMS=100, INITIAL_WINDOW_SIZE=1 MiB.
    try frame(4,0,0,Data([0,3,0,0,0,100,0,4,0,16,0,0]))
    try frame(8,0,0,bigEndian(1_048_576-65_535))
    try frame(1,4,1,Data())
    stage="client stream initialization"
    var empty=Data()
    for value in [UInt32(0x42133742),5,0xf000,4,0] { append(UInt64(value),4,&empty) }
    try message(stream:1,flags:1,body:empty)
    try frame(1,4,3,Data())
    try message(stream:3,flags:0x400001)
    try message(stream:1,flags:0x201)
    _ = try packet(stream:1)
    stage="server stream initialization"
    _ = try packet(stream:3)
    stage="stream activation"
    _ = try packet(stream:1)
    stage="directory handshake"
    let body=try packet(stream:1)
    let decoder=DirectoryDecoder(body)
    guard try decoder.integer(4)==0x42133742,try decoder.integer(4)==5,
          let result=try decoder.object() as? [String:Any],
          result["MessageType"] as? String=="Handshake" else {
      throw PhoneError("Invalid service-directory handshake")
    }
    // Intentionally do not answer with a host handshake.
    return result
  }

  private func append(_ value:UInt64,_ count:Int,_ data:inout Data) {
    for i in 0..<count { data.append(UInt8(truncatingIfNeeded:value >> (i*8))) }
  }
  private func bigEndian(_ value:UInt32) -> Data {
    Data([UInt8(truncatingIfNeeded:value>>24),UInt8(truncatingIfNeeded:value>>16),
          UInt8(truncatingIfNeeded:value>>8),UInt8(truncatingIfNeeded:value)])
  }
  private func frame(_ type:UInt8,_ flags:UInt8,_ stream:UInt32,_ payload:Data) throws {
    let count=payload.count
    var data=Data([UInt8(truncatingIfNeeded:count>>16),UInt8(truncatingIfNeeded:count>>8),
                   UInt8(truncatingIfNeeded:count),type,flags])
    data.append(bigEndian(stream));data.append(payload)
    try send(data)
  }
  private func send(_ data:Data) throws {
    try data.withUnsafeBytes { bytes in
      var offset=0
      while offset<bytes.count {
        let written=Darwin.send(socket.fileDescriptor,bytes.baseAddress!.advanced(by:offset),bytes.count-offset,0)
        if written<0 && errno==EINTR { continue }
        guard written>0 else { throw PhoneError("Service discovery write failed: \(String(cString:strerror(errno)))") }
        offset+=written
      }
    }
  }
  private func message(stream:UInt32,flags:UInt32,body:Data=Data()) throws {
    var data=Data()
    append(0x29b00b92,4,&data);append(UInt64(flags),4,&data)
    append(UInt64(body.count),8,&data);append(0,8,&data);data.append(body)
    try frame(0,0,stream,data)
  }
  private func receive(_ count:Int) throws -> Data {
    var data=Data()
    while data.count<count {
      let remaining=deadline-ProcessInfo.processInfo.systemUptime
      guard remaining>0 else { throw AccessibilityFailure.timeout("service discovery") }
      var descriptor=pollfd(fd:socket.fileDescriptor,events:Int16(POLLIN),revents:0)
      let ready=poll(&descriptor,1,Int32(remaining*1000))
      if ready<0 && errno==EINTR { continue }
      guard ready>0 else { throw AccessibilityFailure.timeout("service discovery") }
      var chunk=Data(count:min(count-data.count,16_384))
      let received=chunk.withUnsafeMutableBytes { Darwin.recv(socket.fileDescriptor,$0.baseAddress,$0.count,0) }
      if received<0 && errno==EINTR { continue }
      if received<0 && (errno==EAGAIN || errno==EWOULDBLOCK) { continue }
      guard received>0 else {
        throw PhoneError("RSD connection closed during \(stage) (read \(received), errno \(errno))")
      }
      data.append(chunk.prefix(received))
    }
    return data
  }
  private func receiveFrame() throws {
    let h=[UInt8](try receive(9))
    let count=Int(h[0])<<16 | Int(h[1])<<8 | Int(h[2])
    guard count<=1_048_576 else { throw PhoneError("Oversized directory frame") }
    let stream=UInt32(h[5]&0x7f)<<24 | UInt32(h[6])<<16 | UInt32(h[7])<<8 | UInt32(h[8])
    let payload=try receive(count)
    switch h[3] {
    case 0:
      guard streams[stream] != nil else { throw PhoneError("Unexpected directory stream") }
      var content=payload
      if h[4]&8 != 0 {
        guard let padding=payload.first,Int(padding)<payload.count else { throw PhoneError("Invalid directory padding") }
        content=Data(payload.dropFirst().dropLast(Int(padding)))
      }
      guard streams[stream]!.count+content.count<=limit else { throw PhoneError("Oversized service directory") }
      streams[stream]!.append(content)
      for id in [UInt32(0),stream] {
        receivedBytes[id,default:0]+=count
        if receivedBytes[id]!>=524_288 {
          try frame(8,0,id,bigEndian(UInt32(receivedBytes[id]!)))
          receivedBytes[id]=0
        }
      }
    case 4:
      guard stream==0,count%6==0 else { throw PhoneError("Invalid HTTP/2 settings") }
      if h[4]&1==0 { try frame(4,1,0,Data()) }
    case 6:
      guard stream==0,count==8 else { throw PhoneError("Invalid HTTP/2 ping") }
      if h[4]&1==0 { try frame(6,1,0,payload) }
    case 3,7: throw PhoneError("RSD stream rejected during \(stage) (HTTP/2 frame \(h[3]))")
    default: break
    }
  }
  private func packet(stream:UInt32) throws -> Data {
    while true {
      let buffer=streams[stream]!
      if buffer.count>=24 {
        let decoder=DirectoryDecoder(Data(buffer.prefix(24)))
        guard try decoder.integer(4)==0x29b00b92 else { throw PhoneError("Invalid directory packet") }
        _ = try decoder.integer(4)
        let length=try decoder.integer(8)
        guard length<=UInt64(limit-24) else { throw PhoneError("Oversized directory packet") }
        if buffer.count>=24+Int(length) {
          streams[stream]=Data(buffer.dropFirst(24+Int(length)))
          return Data(buffer.dropFirst(24).prefix(Int(length)))
        }
      }
      try receiveFrame()
    }
  }
}

/// Bounded decoder for the primitive values in an RSD directory (go-ios XPC v5).
private final class DirectoryDecoder {
  private let bytes:[UInt8]
  private var offset=0
  private var end:Int
  private var objects=0
  init(_ data:Data) { bytes=Array(data);end=data.count }
  private func take(_ count:Int) throws -> ArraySlice<UInt8> {
    guard count>=0,count<=end-offset else { throw PhoneError("Truncated service directory") }
    defer { offset+=count }
    return bytes[offset..<offset+count]
  }
  func integer(_ count:Int) throws -> UInt64 {
    let value=try take(count)
    return value.enumerated().reduce(0) { $0 | UInt64($1.element)<<($1.offset*8) }
  }
  private func padded(_ count:Int) throws -> Data {
    let data=Data(try take(count))
    _ = try take((4-count%4)%4)
    return data
  }
  func object(_ depth:Int=0) throws -> Any {
    objects+=1
    guard depth<32,objects<=20_000 else { throw PhoneError("Service directory is too complex") }
    switch try integer(4) {
    case 0x1000: return NSNull()
    case 0x2000: return try integer(4) != 0
    case 0x3000,0x7000: return Int64(bitPattern:try integer(8))
    case 0x4000: return try integer(8)
    case 0x5000: return Double(bitPattern:try integer(8))
    case 0xa000: return Data(try take(16))
    case 0x8000: return try padded(Int(integer(4)))
    case 0x9000:
      let data=try padded(Int(integer(4)))
      guard let string=String(data:data,encoding:.utf8) else { throw PhoneError("Invalid directory string") }
      return string.trimmingCharacters(in:CharacterSet(charactersIn:"\0"))
    case let type where type==0xe000 || type==0xf000:
      let size=Int(try integer(4))
      guard size>=4,size<=end-offset else { throw PhoneError("Invalid directory container") }
      let previousEnd=end
      end=offset+size
      defer { end=previousEnd }
      let count=Int(try integer(4))
      guard count<=(end-offset)/4 else { throw PhoneError("Invalid directory item count") }
      var dictionary:[String:Any]=[:]
      var array:[Any]=[]
      for _ in 0..<count {
        if type==0xf000 {
          guard let zero=bytes[offset..<end].firstIndex(of:0),
                let key=String(data:try padded(zero-offset+1).dropLast(),encoding:.utf8) else {
            throw PhoneError("Invalid directory key")
          }
          dictionary[key]=try object(depth+1)
        } else { array.append(try object(depth+1)) }
      }
      guard offset==end else { throw PhoneError("Invalid directory container length") }
      return type==0xf000 ? dictionary : array
    default: throw PhoneError("Unsupported service-directory value")
    }
  }
}
