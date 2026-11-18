import Foundation
import AppleBridge

/// Uses macOS's existing CoreDevice tunnel with the kernel UDP socket stack.
/// iOS requires a host display session before it will apply touchscreen reports.
final class Media {
  private let connection: CoreConnection
  private let service = "com.apple.coredevice.displayservice"
  private let fd: Int32
  private var reader: DispatchSourceRead?
  private var timer: DispatchSourceTimer?
  private var started = false
  let video = VideoDecoder()

  init(connection: CoreConnection) throws {
    self.connection = connection
    let peer = try connection.tunnelAddress()
    var host = [CChar](repeating:0,count:46), port: UInt16 = 0
    fd = LPMediaSocket(peer,&host,&port)
    guard fd >= 0 else { throw PhoneError("Cannot bind to macOS's CoreDevice tunnel: \(errno)") }
    let queue = DispatchQueue(label:"lightpinky.media")
    let packets = DispatchSource.makeReadSource(fileDescriptor:fd,queue:queue)
    let descriptor = fd
    let video = video
    packets.setEventHandler {
      var buffer = [UInt8](repeating:0,count:65536)
      while true {
        let count = recv(descriptor,&buffer,buffer.count,0)
        guard count > 0 else { break }
        buffer.withUnsafeBytes { video.receive(UnsafeRawBufferPointer(rebasing:$0[..<count])) }
      }
    }
    packets.setCancelHandler { close(descriptor) }
    packets.resume()
    reader = packets
    do {
      let began = ProcessInfo.processInfo.systemUptime
      let display = try connection.displayInfo()
      let displays = display["displays"] as? [[String:Any]] ?? []
      let displayID = (displays.first(where: { $0["primary"] as? Bool == true })?["displayId"] as? NSNumber)?.int64Value ?? 1
      let options: [String:Any] = [
        "AVCMediaStreamNegotiatorAccessNetworkType":["int":Int64(1)],
        "AVCMediaStreamNegotiatorTransportProtocolType":["int":Int64(2)],
        "CoreDeviceVideoDisplayMode":["string":"DisplayByID"],
        "VideoStreamForDisplayID":["int":displayID],
        "avcMediaStreamOptionClientSessionID":["uuid":UUID()],
      ]
      let response = try connection.invoke(service,feature:"com.apple.coredevice.feature.startmediastream",
        action:"com.apple.coredevice.action.mediastreamstart",input:[
          "clientSupportedFeatures":UInt64(140),"direction":"output","type":"video",
          "negotiatorOffer":try Self.offer(),
          "options":options,
          "receiverIP":String(cString:host),"receiverPort":UInt64(port),
          "senderIP":peer,"timeout":UInt64(10),
        ])
      started = true
      if ProcessInfo.processInfo.environment["LIGHTPINKY_TIMING"] == "1" {
        FileHandle.standardError.write(Data("media start: \(CoreConnection.object(response))\n".utf8))
      }
      if let channel = xpc_dictionary_get_value(response,"connection"),
         let config = xpc_dictionary_get_value(channel,"streamConfig") {
        let sourcePort = UInt16(truncatingIfNeeded:xpc_dictionary_get_uint64(config,"SourcePort"))
        let ssrc = UInt32(truncatingIfNeeded:xpc_dictionary_get_uint64(config,"RemoteSSRC"))
        var report = Data([0x80,201,0,1])
        for shift in [24,16,8,0] { report.append(UInt8(truncatingIfNeeded:ssrc >> shift)) }
        let feedback = DispatchSource.makeTimerSource(queue:queue)
        feedback.schedule(deadline:.now(),repeating:1)
        let trace = ProcessInfo.processInfo.environment["LIGHTPINKY_VIDEO_TRACE"] == "1"
        feedback.setEventHandler {
          LPMediaSend(descriptor,peer,sourcePort,report)
          if trace { FileHandle.standardError.write(Data("video: \(video.stats)\n".utf8)) }
        }
        feedback.resume()
        timer = feedback
      }
      // The acknowledged session enables input. Receiving video is a separate
      // UDP path; keyboard registration and passcode-screen checks still have
      // to succeed before unlock sends any digits.
      Thread.sleep(forTimeInterval:max(0,0.5 - (ProcessInfo.processInfo.systemUptime - began)))
    } catch {
      if started && connection.isValid {
        _ = try? connection.invoke(service,feature:"com.apple.coredevice.feature.stopmediastream",
          action:"com.apple.coredevice.action.mediastreamstop",input:["stopAll":true])
      }
      timer?.cancel()
      packets.cancel()
      throw error
    }
  }

  /// Ends every display stream on the phone, including another process's.
  /// Used before locking, where a live session competes with the side button.
  func stopAll() {
    if started && connection.isValid {
      _ = try? connection.invoke(service,feature:"com.apple.coredevice.feature.stopmediastream",
        action:"com.apple.coredevice.action.mediastreamstop",input:["stopAll":true])
    }
    started = false
  }

  // The stop request has no per-stream form that this phone accepts, and
  // the recorder and the controller each hold a stream. Leaving quietly
  // lets the phone end only this one when its RTCP reports stop arriving.
  deinit {
    timer?.cancel()
    reader?.cancel()
  }

  // Wire layout ported from this repository's former offer.go, originally
  // based on go-ios ios/display/offer.go (MIT, Copyright 2019 danielpaulus).
  private struct Proto {
    var data = Data()
    mutating func raw(_ value: UInt64) {
      var n = value
      while n >= 128 { data.append(UInt8(n & 127) | 128); n >>= 7 }
      data.append(UInt8(n))
    }
    mutating func int(_ field: UInt64,_ value: UInt64) { raw(field << 3); raw(value) }
    mutating func bytes(_ field: UInt64,_ value: Data) {
      raw((field << 3) | 2); raw(UInt64(value.count)); data.append(value)
    }
    mutating func string(_ field: UInt64,_ value: String) { bytes(field,Data(value.utf8)) }
  }

  static func offer() throws -> Data {
    func bank(_ type: UInt64,_ trailer: UInt64,_ pairs: Int) -> Data {
      var bank = Proto()
      bank.int(1,type)
      for i in 0..<pairs {
        var entry = Proto()
        entry.int(1,1); entry.int(2,UInt64(1+i%2)); entry.int(3,50115); entry.int(4,0)
        bank.bytes(2,entry.data)
      }
      bank.string(3,"FLS;SW:1;"); bank.int(4,trailer)
      return bank.data
    }
    var video = Proto()
    video.raw(8)
    var id = UInt64(UInt32.random(in:1...UInt32.max))
    for index in 0..<5 { video.data.append(UInt8(id & 127) | (index < 4 ? 128 : 0)); id >>= 7 }
    video.int(2,1)
    video.bytes(3,bank(123,1,4)); video.bytes(3,bank(100,14,2))
    video.int(7,0); video.int(8,63); video.int(10,1); video.int(12,1)
    var media = Proto()
    media.int(1,1); media.int(2,1); media.bytes(5,video.data); media.string(6,"Viceroy 1.7.0"); media.int(8,0)
    let tiers: [(UInt64,UInt64,UInt64?)] = [
      (4074,0,16384),(0,75000000,524288),(0,40000000,12288),(16,4100,nil),(0,20000000,98304),
      (4,6500,nil),(0,6000000,131072),(0,100000000,1048576),(0,60000000,262144),(1,299,nil),
    ]
    for (kind,bps,buffer) in tiers {
      var tier = Proto(); tier.int(1,kind); tier.int(2,bps)
      if let buffer { tier.int(3,buffer) }
      media.bytes(9,tier.data)
    }
    media.int(13,17137042128614416384); media.int(14,2); media.int(16,0); media.int(18,1)
    var endpoint = Proto()
    endpoint.int(1,0); endpoint.int(2,1); endpoint.string(3,"Mac15,9"); endpoint.string(4,"2205.3.1"); endpoint.string(5,"25F80")
    return try PropertyListSerialization.data(fromPropertyList:[
      "avcMediaStreamOptionRemoteEndpointInfo":endpoint.data,
      "avcMediaStreamNegotiatorMode":5,
      "avcMediaStreamNegotiatorMediaBlob":LPCompress(media.data),
      "avcMediaStreamOptionCallID":UUID().uuidString,
    ],format:.binary,options:0)
  }
}
