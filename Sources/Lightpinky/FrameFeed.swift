import Foundation
import CoreGraphics
import CoreVideo

/// While the controller holds the phone's display stream, it hands its
/// frames to a recorder on this Mac, so the recorder does not pull full
/// screenshots over the same link. The phone allows one stream per display,
/// so the frames have to come from the process that owns it.
///
/// The recorder listens; the controller connects when its stream starts and
/// closes when it ends. Each frame is "LPF1", width, height (little-endian
/// UInt32), then width*height*4 bytes of BGRA.
public enum FrameFeed {
  static func path(udid: String) -> String {
    FileManager.default.homeDirectoryForCurrentUser
      .appendingPathComponent(".cache/lightpinky/run/frames-\(udid).sock").path
  }
  static func address(_ path: String) -> sockaddr_un? {
    guard path.utf8.count < 104 else { return nil }
    var address = sockaddr_un()
    address.sun_family = sa_family_t(AF_UNIX)
    address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
    withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(path.utf8) + [0]) }
    return address
  }
  static func sameUser(_ fd: Int32) -> Bool {
    var uid: uid_t = 0, gid: gid_t = 0
    return getpeereid(fd, &uid, &gid) == 0 && uid == getuid()
  }
}

/// Recorder side. `latest` is nil whenever no controller is streaming.
public final class FrameReceiver {
  private let listener: Int32
  private let path: String
  private let lock = NSLock()
  private var image: CGImage?
  private var receivedAt: Double = 0

  public init(udid: String) throws {
    path = FrameFeed.path(udid: udid)
    let directory = URL(fileURLWithPath: path).deletingLastPathComponent()
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true,
      attributes: [.posixPermissions: 0o700])
    var existing = stat()
    if lstat(path, &existing) == 0 {
      guard existing.st_mode & S_IFMT == S_IFSOCK, existing.st_uid == getuid() else {
        throw PhoneError("Refusing to replace a non-socket path")
      }
      unlink(path)
    }
    listener = socket(AF_UNIX, SOCK_STREAM, 0)
    guard listener >= 0, var address = FrameFeed.address(path) else { throw PhoneError("Cannot create frame socket") }
    let bound = withUnsafePointer(to: &address) {
      $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    guard bound == 0, chmod(path, 0o600) == 0, listen(listener, 2) == 0 else {
      close(listener)
      throw PhoneError("Cannot bind frame socket")
    }
    let descriptor = listener
    Thread.detachNewThread { [weak self] in
      while true {
        let client = accept(descriptor, nil, nil)
        if client < 0 { if errno == EINTR { continue }; return }
        if FrameFeed.sameUser(client) { self?.read(client) }
        close(client)
        self?.store(nil)
      }
    }
  }

  deinit { close(listener); unlink(path) }

  /// The controller's newest frame, if one arrived within `maxAge` seconds.
  public func latest(maxAge: Double) -> CGImage? {
    lock.lock(); defer { lock.unlock() }
    return ProcessInfo.processInfo.systemUptime - receivedAt <= maxAge ? image : nil
  }

  private func store(_ new: CGImage?) {
    lock.lock(); image = new; receivedAt = new == nil ? 0 : ProcessInfo.processInfo.systemUptime; lock.unlock()
  }

  private func read(_ fd: Int32) {
    var timeout = timeval(tv_sec: 5, tv_usec: 0)
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
    func exact(_ count: Int) -> Data? {
      var data = Data(count: count)
      let complete = data.withUnsafeMutableBytes { bytes -> Bool in
        var offset = 0
        while offset < count {
          let n = recv(fd, bytes.baseAddress! + offset, count - offset, 0)
          if n < 0 && errno == EINTR { continue }
          guard n > 0 else { return false }
          offset += n
        }
        return true
      }
      return complete ? data : nil
    }
    while let header = exact(12) {
      guard header.prefix(4) == Data("LPF1".utf8) else { return }
      let width = Int(header[4]) | Int(header[5]) << 8 | Int(header[6]) << 16 | Int(header[7]) << 24
      let height = Int(header[8]) | Int(header[9]) << 8 | Int(header[10]) << 16 | Int(header[11]) << 24
      guard width > 0, height > 0, width <= 8192, height <= 8192, let pixels = exact(width * height * 4),
            let provider = CGDataProvider(data: pixels as CFData),
            let frame = CGImage(width: width, height: height, bitsPerComponent: 8, bitsPerPixel: 32,
              bytesPerRow: width * 4, space: CGColorSpace(name: CGColorSpace.sRGB)!,
              bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.noneSkipFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue),
              provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent) else { return }
      store(frame)
    }
  }
}

/// Controller side. Sending never blocks the stream: a recorder that is
/// absent or slow simply misses frames.
final class FrameSender {
  private let path: String
  private var fd: Int32 = -1

  init(udid: String) { path = FrameFeed.path(udid: udid) }
  deinit { if fd >= 0 { close(fd) } }

  func send(_ buffer: CVPixelBuffer, width: Int, height: Int) {
    if fd < 0 {
      // Connecting to a missing socket fails at once, so retrying each
      // second finds a recorder that started after the controller.
      let candidate = socket(AF_UNIX, SOCK_STREAM, 0)
      guard candidate >= 0, var address = FrameFeed.address(path) else { return }
      let connected = withUnsafePointer(to: &address) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(candidate, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
      }
      guard connected == 0, FrameFeed.sameUser(candidate) else { close(candidate); return }
      var timeout = timeval(tv_sec: 2, tv_usec: 0), on: Int32 = 1
      setsockopt(candidate, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
      setsockopt(candidate, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
      fd = candidate
    }
    guard CVPixelBufferGetPixelFormatType(buffer) == kCVPixelFormatType_32BGRA,
          width <= CVPixelBufferGetWidth(buffer), height <= CVPixelBufferGetHeight(buffer) else { return }
    var frame = Data("LPF1".utf8)
    for value in [UInt32(width), UInt32(height)] { withUnsafeBytes(of: value.littleEndian) { frame.append(contentsOf: $0) } }
    CVPixelBufferLockBaseAddress(buffer, .readOnly)
    let base = CVPixelBufferGetBaseAddress(buffer)!, stride = CVPixelBufferGetBytesPerRow(buffer)
    frame.reserveCapacity(12 + width * height * 4)
    for row in 0..<height { frame.append(Data(bytesNoCopy: base + row * stride, count: width * 4, deallocator: .none)) }
    CVPixelBufferUnlockBaseAddress(buffer, .readOnly)
    let sent = frame.withUnsafeBytes { bytes -> Bool in
      var offset = 0
      while offset < bytes.count {
        let n = Darwin.send(fd, bytes.baseAddress! + offset, bytes.count - offset, 0)
        if n < 0 && errno == EINTR { continue }
        guard n > 0 else { return false }
        offset += n
      }
      return true
    }
    if !sent { close(fd); fd = -1 }
  }
}
