import Foundation
import CoreMedia
import CoreVideo
import VideoToolbox

/// Decodes the phone's RTP/HEVC display stream (RFC 7798) into frames.
/// The stream is a fraction of the size of full PNG screenshots.
/// Depacketization follows pymobiledevice3's screen_stream.py (GPL-free
/// protocol facts only: NAL types, the constant DisplayService trailer).
final class VideoDecoder {
  private let lock = NSLock()
  private var lastSequence: UInt16?
  private var fragment = Data()
  private var unit: [Data] = []
  private var corrupt = false
  private var needKey = true
  private var parameters: [UInt8: Data] = [:]
  private var format: CMVideoFormatDescription?
  private var session: VTDecompressionSession?
  private var latest: CVPixelBuffer?
  private var decodedAt: Double = 0
  private var packetAt: Double = 0
  /// When the picture was last known to be free of gaps.
  private var intactAt = ProcessInfo.processInfo.systemUptime
  private var packets = 0, frames = 0, gaps = 0, keys = 0
  private static let trailer = Data([0x04,0xf0,0x0a,0xc0,0,0,0x03,0,0,0x04,0xec,0x0a,0xb0,0x03])

  deinit { if let session { VTDecompressionSessionInvalidate(session) } }

  /// True once the stream has gone silent, or a gap has gone `patience`
  /// seconds without a keyframe repairing it. Frames normally arrive
  /// continuously, even for a still screen.
  func finished(patience: Double) -> Bool {
    lock.lock(); defer { lock.unlock() }
    let now = ProcessInfo.processInfo.systemUptime
    return packets > 0 && (now - packetAt > patience || (needKey && now - intactAt > patience))
  }

  /// The newest decoded frame, if one was decoded within `maxAge` seconds
  /// or packets are still arriving for an intact picture.
  func frame(maxAge: Double) -> CGImage? {
    lock.lock()
    let now = ProcessInfo.processInfo.systemUptime
    let buffer = !needKey && now - decodedAt <= maxAge ? latest : nil
    lock.unlock()
    guard let buffer else { return nil }
    var image: CGImage?
    VTCreateCGImageFromCVPixelBuffer(buffer, options: nil, imageOut: &image)
    return image
  }

  func receive(_ packet: UnsafeRawBufferPointer) {
    guard packet.count >= 12, packet[0] >> 6 == 2 else { return }
    let type = packet[1] & 0x7f
    if type >= 64 && type <= 95 { return }  // RTCP
    var header = 12 + Int(packet[0] & 0x0f) * 4
    if packet[0] & 0x10 != 0 {
      guard packet.count >= header + 4 else { return }
      header += 4 + (Int(packet[header + 2]) << 8 | Int(packet[header + 3])) * 4
    }
    var end = packet.count
    if packet[0] & 0x20 != 0 { end -= Int(packet[end - 1]) }
    guard end > header + 2 else { return }
    let sequence = UInt16(packet[2]) << 8 | UInt16(packet[3])
    lock.lock(); defer { lock.unlock() }
    packets += 1
    packetAt = ProcessInfo.processInfo.systemUptime
    if let lastSequence, sequence != lastSequence &+ 1 {
      // Never stitch across a gap: VideoToolbox renders partial pictures
      // as artifacts instead of failing.
      gaps += 1; corrupt = true; fragment.removeAll()
    }
    lastSequence = sequence
    let payload = Data(bytes: packet.baseAddress! + header, count: end - header)
    let kind = (payload[0] >> 1) & 0x3f
    if kind == 48 {
      var offset = 2
      while offset + 2 <= payload.count {
        let size = Int(payload[offset]) << 8 | Int(payload[offset + 1])
        offset += 2
        guard size > 0, offset + size <= payload.count else { break }
        unit.append(payload.subdata(in: offset..<(offset + size)))
        offset += size
      }
    } else if kind == 49 {
      guard payload.count > 3 else { return }
      let info = payload[2]
      if info & 0x80 != 0 {
        fragment = Data([(payload[0] & 0x81) | ((info & 0x3f) << 1), payload[1]])
        fragment.append(payload.dropFirst(3))
      } else if !fragment.isEmpty {
        fragment.append(payload.dropFirst(3))
      }
      if info & 0x40 != 0, !fragment.isEmpty { unit.append(Self.strip(fragment)); fragment.removeAll() }
    } else {
      unit.append(Self.strip(payload))
    }
    if packet[1] & 0x80 != 0 { finish() }
  }

  private static func strip(_ nal: Data) -> Data {
    nal.count > trailer.count && nal.suffix(trailer.count) == trailer ? nal.dropLast(trailer.count) : nal
  }

  private func finish() {
    defer { unit.removeAll(); corrupt = false }
    if corrupt { needKey = true; return }
    var slices: [Data] = []
    var key = false
    for nal in unit where nal.count > 2 {
      let kind = (nal[nal.startIndex] >> 1) & 0x3f
      if (32...34).contains(kind) {
        if parameters[kind] != nal { parameters[kind] = Data(nal); format = nil }
      } else if kind < 32 {
        if (16...21).contains(kind) { key = true }
        slices.append(nal)
      }
    }
    guard !slices.isEmpty, key || !needKey else { return }
    if format == nil {
      guard key, let vps = parameters[32], let sps = parameters[33], let pps = parameters[34] else {
        needKey = true; return
      }
      if let session { VTDecompressionSessionInvalidate(session) }
      session = nil
      let sets = [vps, sps, pps]
      var created: CMVideoFormatDescription?
      sets[0].withUnsafeBytes { a in sets[1].withUnsafeBytes { b in sets[2].withUnsafeBytes { c in
        let pointers = [a, b, c].map { $0.baseAddress!.assumingMemoryBound(to: UInt8.self) }
        _ = CMVideoFormatDescriptionCreateFromHEVCParameterSets(allocator: nil, parameterSetCount: 3,
          parameterSetPointers: pointers, parameterSetSizes: sets.map(\.count), nalUnitHeaderLength: 4,
          extensions: nil, formatDescriptionOut: &created)
      } } }
      guard let created else { needKey = true; return }
      format = created
      let attributes = [kCVPixelBufferPixelFormatTypeKey: kCVPixelFormatType_32BGRA] as CFDictionary
      VTDecompressionSessionCreate(allocator: nil, formatDescription: created, decoderSpecification: nil,
        imageBufferAttributes: attributes, outputCallback: nil, decompressionSessionOut: &session)
    }
    guard let session, let format else { needKey = true; return }
    var sample = Data()
    for nal in slices {
      var size = UInt32(nal.count).bigEndian
      withUnsafeBytes(of: &size) { sample.append(contentsOf: $0) }
      sample.append(nal)
    }
    var block: CMBlockBuffer?
    guard CMBlockBufferCreateWithMemoryBlock(allocator: nil, memoryBlock: nil, blockLength: sample.count,
            blockAllocator: nil, customBlockSource: nil, offsetToData: 0, dataLength: sample.count,
            flags: kCMBlockBufferAssureMemoryNowFlag, blockBufferOut: &block) == noErr, let block,
          sample.withUnsafeBytes({ CMBlockBufferReplaceDataBytes(with: $0.baseAddress!, blockBuffer: block,
            offsetIntoDestination: 0, dataLength: sample.count) }) == noErr else { needKey = true; return }
    var buffer: CMSampleBuffer?
    var sizes = [sample.count]
    guard CMSampleBufferCreateReady(allocator: nil, dataBuffer: block, formatDescription: format,
            sampleCount: 1, sampleTimingEntryCount: 0, sampleTimingArray: nil, sampleSizeEntryCount: 1,
            sampleSizeArray: &sizes, sampleBufferOut: &buffer) == noErr, let buffer else { needKey = true; return }
    var decoded: CVPixelBuffer?
    // Synchronous: no asynchronous flag, so the handler runs before return.
    let status = VTDecompressionSessionDecodeFrame(session, sampleBuffer: buffer, flags: [], infoFlagsOut: nil) {
      status, _, image, _, _ in if status == noErr { decoded = image }
    }
    guard status == noErr, let decoded else { needKey = true; return }
    latest = decoded
    decodedAt = ProcessInfo.processInfo.systemUptime
    if key || !needKey { intactAt = decodedAt }
    frames += 1
    if key { keys += 1; needKey = false }
  }

  var stats: String {
    lock.lock(); defer { lock.unlock() }
    let size = latest.map { "\(CVPixelBufferGetWidth($0))x\(CVPixelBufferGetHeight($0))" } ?? "none"
    return "packets \(packets) frames \(frames) keys \(keys) gaps \(gaps) waitingForKey \(needKey) size \(size)"
  }
}
