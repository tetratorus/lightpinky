import Foundation
import Darwin

public final class Server {
  let tools = Tools()
  public init() {}
  public func shutdown() { tools.shutdown() }

  public func respond(_ data: Data) -> Data? {
    var id: Any = NSNull()
    func encode(_ body: [String: Any]) -> Data? { try? JSONSerialization.data(withJSONObject: body, options: [.sortedKeys]) }
    do {
      guard let request = try JSONSerialization.jsonObject(with: data) as? [String: Any],
            request["jsonrpc"] as? String == "2.0", let method = request["method"] as? String else {
        return encode(["jsonrpc":"2.0","id":id,"error":["code":-32600,"message":"Invalid request"]])
      }
      guard let requestID = request["id"] else { return nil }
      id = requestID
      let params = request["params"] as? [String: Any] ?? [:]
      let result: [String: Any]
      switch method {
      case "initialize":
        result = ["protocolVersion":"2024-11-05","capabilities":["tools":[:]],
                  "serverInfo":["name":"lightpinky","version":"1.0.0-swift"]]
      case "ping": result = [:]
      case "tools/list": result = ["tools":Tools.schema()]
      case "tools/call":
        do {
          guard let name = params["name"] as? String else { throw PhoneError("Missing tool name") }
          result = try tools.call(name, params["arguments"] as? [String: Any] ?? [:])
        } catch {
          result = ["isError":true,"content":[["type":"text","text":error.localizedDescription]]]
        }
      default:
        return encode(["jsonrpc":"2.0","id":id,"error":["code":-32601,"message":"Method not found"]])
      }
      return encode(["jsonrpc":"2.0","id":id,"result":result])
    } catch {
      return encode(["jsonrpc":"2.0","id":id,"error":["code":-32700,"message":"Invalid JSON"]])
    }
  }

  public func stdio() {
    while let line = readLine() {
      autoreleasepool {
        if let response = respond(Data(line.utf8)) {
          FileHandle.standardOutput.write(response + Data([10]))
        }
      }
    }
  }

  public func socket(path: String, idle: Double) throws {
    guard path.utf8.count < 104 else { throw PhoneError("Unix socket path is too long") }
    let directory = URL(fileURLWithPath:path).deletingLastPathComponent()
    let attributes = try FileManager.default.attributesOfItem(atPath: directory.path)
    guard (attributes[.ownerAccountID] as? NSNumber)?.uint32Value == getuid(),
          ((attributes[.posixPermissions] as? NSNumber)?.intValue ?? 0) & 0o077 == 0 else {
      throw PhoneError("Socket parent directory must belong to this user and have mode 700")
    }
    // The client serializes daemon startup. Never replace a non-socket file.
    var existing = stat()
    if lstat(path, &existing) == 0 {
      guard existing.st_mode & S_IFMT == S_IFSOCK, existing.st_uid == getuid() else {
        throw PhoneError("Refusing to replace a non-socket path")
      }
      unlink(path)
    }
    let listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
    guard listener >= 0 else { throw PhoneError("Cannot create Unix socket") }
    defer { close(listener); unlink(path) }
    var address = sockaddr_un()
    address.sun_family = sa_family_t(AF_UNIX)
    address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
    withUnsafeMutableBytes(of:&address.sun_path) { buffer in
      buffer.copyBytes(from: Array(path.utf8) + [0])
    }
    let bound = withUnsafePointer(to:&address) {
      $0.withMemoryRebound(to:sockaddr.self,capacity:1) { bind(listener,$0,socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    guard bound == 0, chmod(path,0o600) == 0, listen(listener,8) == 0 else { throw PhoneError("Cannot bind private Unix socket") }
    var lastRequest = ProcessInfo.processInfo.systemUptime
    while true {
      var ready = pollfd(fd:listener,events:Int16(POLLIN),revents:0)
      let polled = poll(&ready,1,1000)
      if idle > 0 && ProcessInfo.processInfo.systemUptime - lastRequest >= idle { return }
      if polled < 0 && errno != EINTR { throw PhoneError("Socket poll failed") }
      if polled <= 0 { continue }
      let client = accept(listener,nil,nil)
      if client < 0 { continue }
      autoreleasepool {
        defer { close(client) }
        var timeout = timeval(tv_sec:10,tv_usec:0)
        setsockopt(client,SOL_SOCKET,SO_RCVTIMEO,&timeout,socklen_t(MemoryLayout<timeval>.size))
        setsockopt(client,SOL_SOCKET,SO_SNDTIMEO,&timeout,socklen_t(MemoryLayout<timeval>.size))
        var uid: uid_t = 0, gid: gid_t = 0
        guard getpeereid(client,&uid,&gid) == 0, uid == getuid() else { return }
        do {
          guard let body = try readHTTP(client) else { return }
          lastRequest = ProcessInfo.processInfo.systemUptime
          let response = respond(body) ?? Data()
          let status = response.isEmpty ? "202 Accepted" : "200 OK"
          let header = Data("HTTP/1.1 \(status)\r\nContent-Type: application/json\r\nContent-Length: \(response.count)\r\nConnection: close\r\n\r\n".utf8)
          try sendAll(client,header + response)
          lastRequest = ProcessInfo.processInfo.systemUptime
        } catch {
          let body = Data("Invalid MCP HTTP request".utf8)
          try? sendAll(client,Data("HTTP/1.1 400 Bad Request\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n".utf8) + body)
        }
      }
    }
  }

  private func readHTTP(_ fd: Int32) throws -> Data? {
    var data = Data()
    var bytes = [UInt8](repeating:0,count:8192)
    while data.count <= 1_048_576 {
      let count = recv(fd,&bytes,bytes.count,0)
      if count == 0 && data.isEmpty { return nil } // client readiness probe
      guard count > 0 else { throw PhoneError("Incomplete HTTP request") }
      data.append(contentsOf: bytes.prefix(count))
      guard let split = data.range(of:Data("\r\n\r\n".utf8)) else { continue }
      guard split.lowerBound < 16384,
            let header = String(data:data[..<split.lowerBound],encoding:.utf8) else { throw PhoneError("Invalid HTTP headers") }
      let lines = header.components(separatedBy:"\r\n")
      guard lines.first == "POST /mcp HTTP/1.1",
            !lines.contains(where: {$0.lowercased().hasPrefix("transfer-encoding:")}) else { throw PhoneError("POST /mcp required") }
      let lengths = lines.filter { $0.lowercased().hasPrefix("content-length:") }
      guard lengths.count == 1, let length = Int(lengths[0].dropFirst("content-length:".count).trimmingCharacters(in:.whitespaces)),
            length >= 0, length <= 1_000_000 else { throw PhoneError("Invalid Content-Length") }
      if data.count - split.upperBound >= length { return data.subdata(in:split.upperBound..<(split.upperBound + length)) }
    }
    throw PhoneError("Request too large")
  }

  private func sendAll(_ fd: Int32, _ data: Data) throws {
    try data.withUnsafeBytes { buffer in
      var offset = 0
      while offset < buffer.count {
        let n = send(fd,buffer.baseAddress!.advanced(by:offset),buffer.count-offset,0)
        guard n > 0 else { throw PhoneError("Socket write failed") }
        offset += n
      }
    }
  }
}
