import Foundation
import ImageIO
import UniformTypeIdentifiers
import AppleBridge

struct PhoneInfo {
  let id: String
  let udid: String
  let name: String
  let transport: String
  var json: [String: Any] {
    ["id": udid, "coredevice_id": id, "name": name, "platform": "ios",
     "backend": "coredevice", "connection": transport, "state": "paired", "virtual": false]
  }

  static func discover() throws -> [PhoneInfo] {
    let result = try Command.device(["list", "devices"])
    return (result["devices"] as? [[String: Any]] ?? []).compactMap { entry in
      let hardware = entry["hardwareProperties"] as? [String: Any] ?? [:]
      guard hardware["reality"] as? String == "physical",
            hardware["platform"] as? String == "iOS",
            let id = entry["identifier"] as? String,
            let udid = hardware["udid"] as? String else { return nil }
      let state = entry["deviceProperties"] as? [String: Any] ?? [:]
      let connection = entry["connectionProperties"] as? [String: Any] ?? [:]
      return PhoneInfo(id: id, udid: udid, name: state["name"] as? String ?? udid,
                       transport: connection["transportType"] as? String ?? "unknown")
    }
  }
}

final class Phone {
  let info: PhoneInfo
  private var hidConnection: HID?
  private var axConnection: CoreConnection?
  private var axSession: AccessibilitySession?
  private var lockStateReader: LockStateReader?
  init(_ info: PhoneInfo) { self.info = info }
  deinit { axSession?.close() }

  private func nativeConnection() throws -> CoreConnection {
    if let axConnection, axConnection.isValid { return axConnection }
    axSession?.close()
    axSession = nil
    lockStateReader = nil
    hidConnection = nil
    axConnection = nil
    let connection = try CoreConnection(identifier:info.id, udid:info.udid)
    axConnection = connection
    return connection
  }

  func hid() throws -> HID {
    let connection = try nativeConnection()
    if let hidConnection { return hidConnection }
    let hid = HID(connection:connection)
    hidConnection = hid
    return hid
  }

  func locked() throws -> Bool {
    let connection = try nativeConnection()
    do {
      if lockStateReader == nil { lockStateReader = try LockStateReader(connection: connection) }
      return try lockStateReader!.locked()
    } catch {
      lockStateReader = nil
      connection.invalidateDirectory()
      throw error
    }
  }

  func display() throws -> [String: Any] {
    let result = try nativeConnection().displayInfo()
    guard let displays = result["displays"] as? [[String: Any]],
          var display = displays.first(where: { $0["primary"] as? Bool == true }) else {
      throw PhoneError("No primary display")
    }
    display["backlightState"] = display["backlightState"] ?? result["backlightState"]
    return display
  }

  func power() throws -> [String: Any] {
    let light = try display()["backlightState"] as? String ?? ""
    return ["Locked": try locked(), "ScreenOn": ["on", "activeOn", "activeDimmed"].contains(light)]
  }

  func lock() throws {
    _ = try nativeConnection()
    let before = try power()
    if before["Locked"] as? Bool == true && before["ScreenOn"] as? Bool == false { return }
    // End the developer display/input session before putting the phone to
    // sleep. Keeping it alive can compete with the side-button transition.
    hidConnection = nil
    // Consumer Power is the side button. Send it once, then verify instead
    // of toggling it again if the reply is delayed.
    try hid().button(usage: 0x30)
    let deadline = ProcessInfo.processInfo.systemUptime + 6
    var confirmedAt: Double?
    repeat {
      Thread.sleep(forTimeInterval: 0.15)
      let state = try power()
      if state["Locked"] as? Bool == true && state["ScreenOn"] as? Bool == false {
        let now = ProcessInfo.processInfo.systemUptime
        if let confirmedAt, now - confirmedAt >= 0.5 { return }
        if confirmedAt == nil { confirmedAt = now }
      } else { confirmedAt = nil }
    } while ProcessInfo.processInfo.systemUptime < deadline
    throw PhoneError("Lock button sent once, but the phone did not stay locked with its display off")
  }

  func wake() throws {
    _ = try nativeConnection()
    if try power()["ScreenOn"] as? Bool == true { return }
    try hid().button(usage: 0x40)
    Thread.sleep(forTimeInterval: 0.6)
    guard try power()["ScreenOn"] as? Bool == true else { throw PhoneError("Wake sent but the display stayed off") }
  }

  func screenshot() throws -> CGImage {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent("lightpinky-shot-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                           attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: directory) }
    let path = directory.appendingPathComponent("screen.png")
    for attempt in 0..<4 {
      do {
        _ = try Command.device(["device", "capture", "screenshot", "--destination", path.path], id: info.id)
        break
      } catch {
        // The passive recorder can briefly own the device's screenshot slot.
        guard attempt < 3, error.localizedDescription.contains("25003") else { throw error }
        Thread.sleep(forTimeInterval:0.2)
      }
    }
    // ImageIO decodes lazily. Own the encoded bytes before removing the
    // temporary file, otherwise subsequent OCR/JPEG encoding loses the pixels.
    let encoded = try Data(contentsOf:path)
    guard let source = CGImageSourceCreateWithData(encoded as CFData, nil),
          let image = CGImageSourceCreateImageAtIndex(source, 0,
            [kCGImageSourceShouldCacheImmediately:true] as CFDictionary) else { throw PhoneError("Cannot decode screenshot") }
    return image
  }

  static func jpeg(_ image: CGImage, maxSize: Int) throws -> ([String: Any], String) {
    let factor = maxSize > 0 ? max(1, Int(ceil(Double(max(image.width, image.height)) / Double(maxSize)))) : 1
    var scaled = image
    if factor > 1 {
      guard let context = CGContext(data: nil, width: max(1, image.width / factor), height: max(1, image.height / factor),
          bitsPerComponent: 8, bytesPerRow: 0, space: CGColorSpaceCreateDeviceRGB(),
          bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else { throw PhoneError("Cannot resize screenshot") }
      context.draw(image, in: CGRect(x: 0, y: 0, width: context.width, height: context.height))
      scaled = context.makeImage()!
    }
    let data = NSMutableData()
    guard let dest = CGImageDestinationCreateWithData(data, UTType.jpeg.identifier as CFString, 1, nil) else {
      throw PhoneError("Cannot encode screenshot")
    }
    CGImageDestinationAddImage(dest, scaled, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
    guard CGImageDestinationFinalize(dest) else { throw PhoneError("JPEG encoding failed") }
    return (["type": "image", "mimeType": "image/jpeg", "data": (data as Data).base64EncodedString()],
      "image \(scaled.width)x\(scaled.height) of a \(image.width)x\(image.height) screen; multiply image coordinates by \(factor) for tap and swipe")
  }

  func panelPoint(x: Double, y: Double, display: [String: Any]) throws -> CGPoint {
    guard let bounds = display["bounds"] as? [[Double]], bounds.count == 2, bounds[1].count == 2 else {
      throw PhoneError("Display dimensions unavailable")
    }
    let w = bounds[1][0], h = bounds[1][1]
    let rotation = display["currentOrientation"] as? String ?? "rot0"
    let sideways = ["rot90", "rot270"].contains(rotation)
    guard x.isFinite, y.isFinite, x >= 0, y >= 0, x < (sideways ? h : w), y < (sideways ? w : h) else {
      throw PhoneError("Point is outside the screenshot")
    }
    let p = CGPoint(x: x / (sideways ? h : w), y: y / (sideways ? w : h))
    switch rotation {
    case "rot90": return CGPoint(x: p.y, y: 1 - p.x)
    case "rot180": return CGPoint(x: 1 - p.x, y: 1 - p.y)
    case "rot270": return CGPoint(x: 1 - p.y, y: p.x)
    default: return p
    }
  }

  func gesture(x1: Double, y1: Double, x2: Double, y2: Double, duration: Double) throws {
    guard duration.isFinite, duration > 0, duration <= 10 else { throw PhoneError("Duration must be above 0 and at most 10 seconds") }
    let display = try display()
    try hid().gesture(from: panelPoint(x: x1, y: y1, display: display),
                      to: panelPoint(x: x2, y: y2, display: display), duration: duration)
  }

  func unlock() throws {
    let started = ProcessInfo.processInfo.systemUptime
    func trace(_ stage: String) {
      guard ProcessInfo.processInfo.environment["LIGHTPINKY_TIMING"] == "1" else { return }
      let elapsed = ProcessInfo.processInfo.systemUptime - started
      FileHandle.standardError.write(Data(String(format: "unlock %.3fs %@\n", elapsed, stage).utf8))
    }
    // Explicit control must establish the paired developer connection before
    // querying a locked Wi-Fi phone. A passive lock-state query cannot do that.
    _ = try nativeConnection()
    trace("connected")
    // PasswordProtected briefly lags a side-button transition. An unlock
    // must wake the display even if that first lock-state read says false.
    try wake()
    if try !locked() { return }
    trace("locked")
    // iOS can end the developer display session when the phone locks.
    // Recreate input services before beginning a new unlock attempt.
    hidConnection = nil
    guard let code = ProcessInfo.processInfo.environment["IPHONE_UNLOCK_CODE"], !code.isEmpty else {
      throw PhoneError("Set IPHONE_UNLOCK_CODE locally in the server environment or ~/.zshrc; never pass it as a tool argument")
    }
    // Validate the full code before any passcode input. This Mac uses a numeric PIN.
    guard code.allSatisfy({ $0.isASCII && $0.isNumber }) else {
      throw PhoneError("This native unlock implementation currently requires a numeric passcode")
    }
    try wake()
    trace("awake")
    let input = try hid()
    try input.prepareKeyboard()
    trace("keyboard ready")
    if try !locked() { return }
    try input.keys([0x28])
    Thread.sleep(forTimeInterval: 0.6)
    if try !locked() { return }
    func passcodeVisible() throws -> Bool {
      let screen=try elements(preferPush: true)
      let labels=screen.elements.compactMap { $0["Label"] as? String }
      if labels.contains(where: {
        let text=$0.lowercased()
        return text.hasPrefix("iphone unavailable") || text.hasPrefix("security lockout") || text.contains("try again in")
      }) {
        throw PhoneError("Phone is not accepting a passcode; no code was entered")
      }
      guard screen.json["Locked"] as? Bool == true,
            labels.contains(where: { $0=="Enter Passcode" || $0=="Swipe up for Face ID or Enter Passcode" }),
            let field=screen.elements.first(where: {
              ($0["Label"] as? String)?.hasPrefix("Passcode field, ") == true &&
              ($0["States"] as? [String])?.contains("focused") == true
            }),
            (0...9).allSatisfy({ labels.contains(String($0)) }) else { return false }
      guard field["Label"] as? String == "Passcode field, 0 of \(code.count) values entered" else {
        throw PhoneError("The passcode field is not empty or its length does not match; no code was entered")
      }
      return true
    }
    var ready = try passcodeVisible()
    trace("screen inspected")
    if !ready {
      // Fall back to the lock-screen swipe if Return did not present the pad.
      // Face ID can animate before the numeric pad becomes visible.
      if try !locked() { return }
      try input.gesture(from: CGPoint(x: 0.5, y: 0.9996), to: CGPoint(x: 0.5, y: 0.5), duration: 0.3)
      let deadline = ProcessInfo.processInfo.systemUptime + 8
      repeat {
        Thread.sleep(forTimeInterval:0.5)
        if try !locked() { return }
        ready = try passcodeVisible()
      } while !ready && ProcessInfo.processInfo.systemUptime < deadline
    }
    guard ready else {
      throw PhoneError("System passcode entry screen was not confirmed; no code was entered")
    }
    trace("passcode screen confirmed")
    try input.prepareKeyboard()
    // From this point onward, never retry. Check lock state before each key so
    // a Face ID unlock cannot spill the rest of the code into the foreground app.
    var entered = 0
    do {
      for digit in code.utf8 {
        if try !locked() { return }
        try input.keys([digit == 48 ? 0x27 : 0x1e + digit - 49])
        entered += 1
        Thread.sleep(forTimeInterval: 0.12)
      }
      trace("input complete")
      if code.count != 4 && code.count != 6, try locked() { try input.keys([0x28]) }
      for _ in 0..<8 {
        Thread.sleep(forTimeInterval: 0.3)
        if try !locked() { trace("unlocked"); return }
      }
    } catch {
      if entered > 0 {
        throw PhoneError("Passcode entry interrupted after \(entered) of \(code.count) digits (\(error.localizedDescription)); no retry was made")
      }
      throw error
    }
    throw PhoneError("One passcode attempt completed, but the phone remains locked. No retry was made.")
  }

  func key(_ key: String) throws {
    switch key {
    case "home": try hid().button(usage: 0x40)
    case "back":
      try hid().gesture(from: CGPoint(x: 0, y: 0.5), to: CGPoint(x: 0.7, y: 0.5), duration: 0.25)
    default:
      let usages: [String: UInt8] = ["enter":0x28, "tab":0x2b, "delete":0x2a, "forward_delete":0x4c,
        "escape":0x29, "dpad_up":0x52, "dpad_down":0x51, "dpad_left":0x50, "dpad_right":0x4f,
        "page_up":0x4b, "page_down":0x4e]
      guard let usage = usages[key] else { throw PhoneError("Unknown key \(key)") }
      try hid().keys([usage])
    }
  }

  func type(_ text: String, append: Bool, submit: Bool) throws {
    guard try !locked() else { throw PhoneError("Phone is locked; use unlock") }
    let keyboard = try hid()
    try keyboard.prepareKeyboard()
    if !text.isEmpty {
      _ = try Command.device(["device", "pasteboard", "copy"], id: info.id, input: Data(text.utf8))
    }
    if !append { try keyboard.keys([0xe3, 0x04]); Thread.sleep(forTimeInterval: 0.1) }
    if !text.isEmpty { try keyboard.keys([0xe3, 0x19]) }
    else if !append { try keyboard.keys([0x2a]) }
    if submit { Thread.sleep(forTimeInterval: 0.1); try keyboard.keys([0x28]) }
  }

  func apps() throws -> [[String: Any]] {
    let result = try Command.device(["device", "info", "apps", "--include-all-apps"], id: info.id)
    return (result["apps"] as? [[String: Any]] ?? []).filter {
      $0["hidden"] as? Bool != true && $0["internalApp"] as? Bool != true && $0["appClip"] as? Bool != true
    }
  }

  func appID(_ name: String) throws -> String {
    let apps = try apps()
    let exact = apps.filter { $0["bundleIdentifier"] as? String == name }
    func normalized(_ value:String) -> String {
      let visible=value.unicodeScalars.filter { !$0.properties.isBidiControl && $0.value != 0xfeff }
      return String(String.UnicodeScalarView(visible)).precomposedStringWithCanonicalMapping
        .trimmingCharacters(in:.whitespacesAndNewlines).lowercased()
    }
    let wanted=normalized(name)
    let matches = exact.isEmpty ? apps.filter {
      guard let displayName=$0["name"] as? String else { return false }
      return normalized(displayName)==wanted
    } : exact
    guard matches.count == 1, let id = matches[0]["bundleIdentifier"] as? String else {
      throw PhoneError("App name is missing or ambiguous; use a bundle identifier from apps")
    }
    return id
  }

  func launchApp(_ name: String) throws {
    let bundle = try appID(name)
    // Wire format from go-ios ios/appservice/appservice.go (MIT; see NOTICE).
    let options = try PropertyListSerialization.data(
      fromPropertyList: [String: Any](), format: .binary, options: 0)
    let result = try nativeConnection().invoke("com.apple.coredevice.appservice",
      feature: "com.apple.coredevice.feature.launchapplication", input: [
        "applicationSpecifier": ["bundleIdentifier": ["_0": bundle]],
        "options": [
          "arguments": [Any](), "environmentVariables": [String: Any](),
          "platformSpecificOptions": options,
          "standardIOUsesPseudoterminals": true, "startStopped": false,
          "terminateExisting": false, "user": ["active": true],
          "workingDirectory": NSNull(),
        ],
        "standardIOIdentifiers": [String: Any](),
      ])
    guard let output = CoreConnection.object(result) as? [String: Any],
          let process = output["processToken"] as? [String: Any],
          let pid = process["processIdentifier"] as? NSNumber, pid.intValue > 0 else {
      throw PhoneError("App launch did not return a process identifier")
    }
  }

  func stopApp(_ name: String) throws {
    let id = try appID(name)
    guard let app = try apps().first(where: { $0["bundleIdentifier"] as? String == id }),
          let urlString = app["url"] as? String, let bundle = URL(string:urlString) else {
      throw PhoneError("The app bundle path is unavailable")
    }
    let processes = try Command.device(["device","info","processes"],id:info.id)
    for process in processes["runningProcesses"] as? [[String:Any]] ?? [] {
      guard let executable = process["executable"] as? String,
            let path = URL(string:executable),
            path.deletingLastPathComponent().standardized.path == bundle.standardized.path,
            let pid = process["processIdentifier"] as? NSNumber, pid.intValue > 0 else { continue }
      _ = try Command.device(["device","process","terminate","--pid",pid.stringValue],id:info.id)
    }
  }

  func elements(preferPush: Bool = false) throws -> Screen {
    // Only observation is repeated. Input tools never replay a gesture or
    // passcode after a timeout.
    for attempt in 0..<2 {
      do {
        let connection = try nativeConnection()
        if axSession == nil {
          let socket = try connection.socket("com.apple.accessibility.axAuditDaemon.remoteAXService")
          axSession = try AccessibilitySession(socket:socket)
        }
        return try Screen(responses:axSession!.tree(preferPush: preferPush),scale:display()["scale"] as? Double ?? 3,locked:locked())
      } catch {
        axSession?.close()
        axSession=nil
        guard attempt==0, let failure=error as? AccessibilityFailure else { throw error }
        // A foreground app transition can invalidate an AX object (error 8).
        // Restart observation from the new foreground app, never replay input.
        if case .rejected(let code) = failure, code != 8 { throw error }
        if case .disconnected = failure { axConnection?.invalidateDirectory() }
      }
    }
    throw PhoneError("Accessibility unavailable")
  }

  func openURL(_ url: String, app: String? = nil) throws {
    guard let parsed = URL(string: url), parsed.scheme != nil else { throw PhoneError("URL requires a scheme") }
    let handler = try app.map { try appID($0) } ?? "com.apple.mobilesafari"
    _ = try Command.device(["device","process","launch","--payload-url",url,handler],id:info.id)
  }
}
