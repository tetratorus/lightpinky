import Foundation

struct Command {
  static func run(_ arguments: [String], input: Data? = nil, timeout: Double = 20, executable: String = "/usr/bin/xcrun") throws -> Data {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent("lightpinky-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                           attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: directory) }
    let out = directory.appendingPathComponent("stdout")
    let err = directory.appendingPathComponent("stderr")
    FileManager.default.createFile(atPath: out.path, contents: nil)
    FileManager.default.createFile(atPath: err.path, contents: nil)
    let output = try FileHandle(forWritingTo: out), errors = try FileHandle(forWritingTo: err)
    defer { try? output.close(); try? errors.close() }
    let process = Process()
    process.executableURL = URL(fileURLWithPath: executable)
    process.arguments = arguments
    process.standardOutput = output
    process.standardError = errors
    if let input {
      let source = directory.appendingPathComponent("stdin")
      try input.write(to: source)
      process.standardInput = try FileHandle(forReadingFrom: source)
    } else { process.standardInput = FileHandle.nullDevice }
    try process.run()
    let deadline = ProcessInfo.processInfo.systemUptime + timeout
    while process.isRunning && ProcessInfo.processInfo.systemUptime < deadline {
      Thread.sleep(forTimeInterval: 0.02)
    }
    if process.isRunning {
      process.terminate()
      Thread.sleep(forTimeInterval: 0.1)
      if process.isRunning { kill(process.processIdentifier, SIGKILL) }
      process.waitUntilExit()
      throw PhoneError("Apple developer command timed out; the operation was not retried")
    }
    process.waitUntilExit()
    guard process.terminationStatus == 0 else {
      throw PhoneError(String(decoding: try Data(contentsOf: err), as: UTF8.self))
    }
    return try Data(contentsOf: out)
  }

  static func device(_ args: [String], id: String? = nil, input: Data? = nil) throws -> [String: Any] {
    // process launch consumes everything after the bundle ID as app arguments.
    // Put devicectl options before any positional arguments.
    var command = ["devicectl"] + args.prefix(3)
    if let id { command += ["--device", id] }
    command += ["--quiet", "--timeout", "15", "--json-output", "-"]
    command += args.dropFirst(3)
    let data:Data
    do { data=try run(command,input:input) }
    catch { throw PhoneError("\(args.prefix(3).joined(separator:" ")): \(error.localizedDescription)") }
    guard let json = try JSONSerialization.jsonObject(with: data) as? [String: Any],
          let result = json["result"] as? [String: Any] else { throw PhoneError("Invalid devicectl result") }
    return result
  }
}
