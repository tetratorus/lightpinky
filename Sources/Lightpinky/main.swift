import Foundation
import AppleBridge

umask(0o077)
signal(SIGPIPE, SIG_IGN)

// The command loop runs off the main queue because Apple's device services
// deliver callbacks on the process main run loop.
DispatchQueue.global().async {
  do {
    var args = CommandLine.arguments.dropFirst().makeIterator()
    var socketPath: String?, idle = 0.0
    while let argument = args.next() {
      switch argument {
      case "-socket":
        guard let path = args.next() else { throw PhoneError("-socket needs a path") }
        socketPath = path
      case "-idle":
        guard let value = args.next() else { throw PhoneError("-idle needs a duration") }
        let multiplier = value.hasSuffix("m") ? 60.0 : value.hasSuffix("h") ? 3600.0 : 1.0
        let numeric = value.last?.isLetter == true ? String(value.dropLast()) : value
        guard let duration = Double(numeric), duration.isFinite, duration >= 0 else { throw PhoneError("Invalid idle duration") }
        idle = duration * multiplier
      case "-h","--help":
        print("lightpinky: native macOS iPhone MCP server\nDefault: JSON-RPC over stdio\n-socket PATH [-idle 15m]: private Unix HTTP MCP server\nDiscover tools with scripts/phone tools")
        exit(0)
      default: throw PhoneError("Unknown option \(argument); use --help")
      }
    }
    let server = Server()
    if let socketPath { try server.socket(path:socketPath,idle:idle) }
    else { server.stdio() }
    server.tools.shutdown()
    exit(0)
  } catch {
    fputs("\(error.localizedDescription)\n", stderr)
    exit(1)
  }
}
RunLoop.main.run()
