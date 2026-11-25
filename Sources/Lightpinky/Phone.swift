import Foundation
import ImageIO
import UniformTypeIdentifiers
import AppleBridge

public struct PhoneInfo {
  public let id: String
  public let udid: String
  public let name: String
  public let transport: String
  var json: [String: Any] {
    ["id": udid, "coredevice_id": id, "name": name, "platform": "ios",
     "backend": "coredevice", "connection": transport, "state": "paired", "virtual": false]
  }

  public static func discover() throws -> [PhoneInfo] {
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

public final class Phone {
  public let info: PhoneInfo
  private var hidConnection: HID?
  private var axConnection: CoreConnection?
  private var axSession: DTXSession?
  private var cachedApps: [[String: Any]]?
  private var appsAt: Double = 0
  public init(_ info: PhoneInfo) { self.info = info }
  deinit { axSession?.close() }

  private func nativeConnection() throws -> CoreConnection {
    if let axConnection, axConnection.isValid { return axConnection }
    axSession?.close()
    axSession = nil
    hidConnection = nil
    activeTouchDisplay = nil
    axConnection = nil
    cachedApps = nil
    appsAt = 0
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

  func locked(recover: Bool = true) throws -> Bool {
    if recover { return try CoreConnectionFailure.retryRead { try nativeConnection().locked() } }
    return try nativeConnection().locked()
  }

  func display() throws -> [String: Any] {
    try Self.primaryDisplay(nativeConnection().displayInfo())
  }

  private static func primaryDisplay(_ result: [String: Any]) throws -> [String: Any] {
    guard let displays = result["displays"] as? [[String: Any]],
          var display = displays.first(where: { $0["primary"] as? Bool == true }) else {
      throw PhoneError("No primary display")
    }
    display["backlightState"] = display["backlightState"] ?? result["backlightState"]
    return display
  }

  public func power() throws -> [String: Any] {
    try CoreConnectionFailure.retryRead {
      // Repeat the whole observation on one connection; don't combine a
      // display read from an old tunnel with a lock read from its replacement.
      let connection = try nativeConnection()
      let light = try Self.primaryDisplay(connection.displayInfo())["backlightState"] as? String ?? ""
      return ["Locked": try connection.locked(), "ScreenOn": ["on", "activeOn", "activeDimmed"].contains(light)]
    }
  }

  private func screenOn() throws -> Bool {
    try CoreConnectionFailure.retryRead {
      let light = try display()["backlightState"] as? String ?? ""
      return ["on", "activeOn", "activeDimmed"].contains(light)
    }
  }

  public func lock() throws {
    _ = try nativeConnection()
    let before = try power()
    if before["Locked"] as? Bool == true && before["ScreenOn"] as? Bool == false { return }
    // End the developer display/input session before putting the phone to
    // sleep. Keeping it alive can compete with the side-button transition.
    hidConnection?.endDisplaySession()
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

  public func wake() throws {
    _ = try nativeConnection()
    if try screenOn() { return }
    try hid().button(usage: 0x40)
    let deadline = ProcessInfo.processInfo.systemUptime + 6
    repeat {
      Thread.sleep(forTimeInterval: 0.15)
      if try screenOn() { return }
    } while ProcessInfo.processInfo.systemUptime < deadline
    throw PhoneError("Wake sent once, but the display did not turn on")
  }

  /// Ends the display stream. The next input starts it again.
  public func endDisplaySession() {
    activeTouchDisplay = nil
    hidConnection?.endDisplaySession()
  }

  func endIdleDisplaySession(after seconds: Double) {
    hidConnection?.endIdleDisplaySession(after: seconds)
  }

  private var panelSize: (Int, Int)?
  private func videoSize() throws -> (Int, Int) {
    if let panelSize { return panelSize }
    guard let bounds = try display()["bounds"] as? [[Double]], bounds.count == 2, bounds[1].count == 2 else {
      throw PhoneError("Display size unavailable")
    }
    panelSize = (Int(bounds[1][0]), Int(bounds[1][1]))
    return panelSize!
  }

  /// A frame from the display video stream, or nil when none is current.
  /// Starts the developer display session; it sends no input to the phone.
  public func videoFrame(wait: Double = 0, maxAge: Double = 2) throws -> CGImage? {
    var video = try hid().video()
    // A finished stream was ended by another process or lost packets.
    if video.finished(patience: 2) {
      hidConnection = nil
      video = try hid().video()
    }
    let deadline = ProcessInfo.processInfo.systemUptime + wait
    while true {
      if let image = video.frame(maxAge: maxAge) {
        if ProcessInfo.processInfo.environment["LIGHTPINKY_TIMING"] == "1" {
          FileHandle.standardError.write(Data("video: \(video.stats)\n".utf8))
        }
        // The encoder pads the picture to a block multiple on the right and
        // bottom. Crop to the panel so coordinates match screenshots.
        let size = try videoSize()
        let rect = CGRect(x: 0, y: 0, width: min(size.0, image.width), height: min(size.1, image.height))
        return image.cropping(to: rect) ?? image
      }
      if ProcessInfo.processInfo.systemUptime >= deadline { break }
      Thread.sleep(forTimeInterval: 0.05)
    }
    if ProcessInfo.processInfo.environment["LIGHTPINKY_TIMING"] == "1" {
      FileHandle.standardError.write(Data("video: \(video.stats)\n".utf8))
    }
    return nil
  }

  public func screenshot() throws -> CGImage {
    let encoded=try nativeConnection().screenshotData()
    guard let source=CGImageSourceCreateWithData(encoded as CFData,nil),
          let image=CGImageSourceCreateImageAtIndex(source,0,
            [kCGImageSourceShouldCacheImmediately:true] as CFDictionary) else {
      throw PhoneError("Cannot decode screenshot")
    }
    return image
  }

  static func jpeg(_ image: CGImage, maxSize: Int, lossless: Bool = false) throws -> ([String: Any], String) {
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
    let type = lossless ? UTType.png : UTType.jpeg
    guard let dest = CGImageDestinationCreateWithData(data, type.identifier as CFString, 1, nil) else {
      throw PhoneError("Cannot encode screenshot")
    }
    CGImageDestinationAddImage(dest, scaled, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
    guard CGImageDestinationFinalize(dest) else { throw PhoneError("Image encoding failed") }
    return (["type": "image", "mimeType": lossless ? "image/png" : "image/jpeg", "data": (data as Data).base64EncodedString()],
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

  private var activeTouchDisplay: [String: Any]?
  public func touch(x: Double, y: Double, down: Bool) throws {
    let info: [String: Any]
    if let active = activeTouchDisplay {
      info = active
    } else {
      info = try display()
      if down { activeTouchDisplay = info }
    }
    if !down { activeTouchDisplay = nil }
    try hid().contact(panelPoint(x: x, y: y, display: info), down: down)
  }

  public func gesture(x1: Double, y1: Double, x2: Double, y2: Double, duration: Double) throws {
    guard duration.isFinite, duration > 0, duration <= 10 else { throw PhoneError("Duration must be above 0 and at most 10 seconds") }
    let display = try display()
    try hid().gesture(from: panelPoint(x: x1, y: y1, display: display),
                      to: panelPoint(x: x2, y: y2, display: display), duration: duration)
  }

  public func unlock() throws {
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
    hidConnection?.endDisplaySession()
    hidConnection = nil
    guard let code = ProcessInfo.processInfo.environment["IPHONE_UNLOCK_CODE"], !code.isEmpty else {
      throw PhoneError("Set IPHONE_UNLOCK_CODE locally in the server environment or ~/.zshrc; never pass it as a tool argument")
    }
    // Validate the full code before any passcode input. This Mac uses a numeric PIN.
    guard code.allSatisfy({ $0.isASCII && $0.isNumber }) else {
      throw PhoneError("This native unlock implementation currently requires a numeric passcode")
    }
    let input = try hid()
    try input.prepareKeyboard()
    trace("keyboard ready")
    if try !locked() { return }
    try input.keys([0x28])
    var lastReadError: String?
    func passcodeScreen() throws -> UnlockReadiness {
      let screen: Screen
      do {
        screen = try elements(preferPush: true)
      } catch let failure as AccessibilityFailure {
        lastReadError = failure.localizedDescription
        return .waiting
      } catch let failure as PhoneError where failure.localizedDescription == "No foreground accessibility application"
          || failure.localizedDescription == "Accessibility did not provide screen bounds" {
        lastReadError = failure.localizedDescription
        return .waiting
      }
      lastReadError = nil
      return try Self.unlockReadiness(locked: screen.json["Locked"] as? Bool,
                                     elements: screen.elements, codeLength: code.count)
    }
    // Return presents the keypad; give the transition a single observation
    // window. Swiping during it can scroll notification history instead.
    let deadline = ProcessInfo.processInfo.systemUptime + 10
    var readiness = UnlockReadiness.waiting
    repeat {
      Thread.sleep(forTimeInterval: 0.4)
      // elements() already supplies a live lock-state read. Don't perform a
      // second round trip before and after the same screen observation.
      readiness = try passcodeScreen()
    } while readiness == .waiting && ProcessInfo.processInfo.systemUptime < deadline
    if readiness == .unlocked { return }
    guard readiness == .passcode else {
      let note = lastReadError.map { " (last screen read: \($0))" } ?? ""
      throw PhoneError("System passcode entry screen was not confirmed\(note); no code was entered")
    }
    trace("passcode screen confirmed")
    // Observation may have re-established the connection while waiting for
    // the keypad. Bind the entire PIN attempt to the current input connection.
    let inputConnection = try nativeConnection()
    let pinInput = try hid()
    try pinInput.prepareKeyboard()
    // From this point onward, never reconnect or replay input. Check lock state before each key so
    // a Face ID unlock cannot spill the rest of the code into the foreground app.
    var entered = 0
    do {
      for digit in code.utf8 {
        if try !inputConnection.locked() { return }
        try pinInput.keys([digit == 48 ? 0x27 : 0x1e + digit - 49])
        entered += 1
        Thread.sleep(forTimeInterval: 0.12)
      }
      trace("input complete")
      if code.count != 4 && code.count != 6, try inputConnection.locked() { try pinInput.keys([0x28]) }
    } catch {
      if entered > 0 {
        throw PhoneError("Passcode entry interrupted after \(entered) of \(code.count) digits (\(error.localizedDescription)); no retry was made")
      }
      throw error
    }
    if try Self.confirmUnlock(locked: { try self.locked(recover: false) }, reconnect: { self.axConnection = nil }) {
      trace("unlocked")
      return
    }
    throw PhoneError("One passcode attempt completed, but the phone remains locked. No retry was made.")
  }

  enum UnlockReadiness { case waiting, unlocked, passcode }

  static func unlockReadiness(locked: Bool?, elements: [[String: Any]],
                              codeLength: Int) throws -> UnlockReadiness {
    if locked == false { return .unlocked }
    guard locked == true else { return .waiting }
    let labels = elements.compactMap { $0["Label"] as? String }
    if labels.contains(where: {
      let text = $0.lowercased()
      return text.hasPrefix("iphone unavailable") || text.hasPrefix("security lockout") || text.contains("try again in")
    }) {
      throw PhoneError("Phone is not accepting a passcode; no code was entered")
    }
    guard labels.contains(where: { $0 == "Enter Passcode" || $0 == "Swipe up for Face ID or Enter Passcode" }),
          let field = elements.first(where: {
            ($0["Label"] as? String)?.hasPrefix("Passcode field, ") == true &&
            ($0["States"] as? [String])?.contains("focused") == true
          }), (0...9).allSatisfy({ labels.contains(String($0)) }) else { return .waiting }
    guard field["Label"] as? String == "Passcode field, 0 of \(codeLength) values entered" else {
      throw PhoneError("The passcode field is not empty or its length does not match; no code was entered")
    }
    return .passcode
  }

  /// Confirmation is observation only; the PIN attempt has already finished.
  static func confirmUnlock(locked: () throws -> Bool, reconnect: () -> Void,
                            now: () -> Double = { ProcessInfo.processInfo.systemUptime },
                            pause: () -> Void = { Thread.sleep(forTimeInterval: 0.3) }) throws -> Bool {
    let deadline = now() + 5
    var reconnected = false
    repeat {
      do {
        if try !locked() { return true }
      } catch {
        guard !reconnected, error is CoreConnectionFailure else { throw error }
        reconnected = true
        reconnect()
        // A timed-out read can consume the entire polling deadline. Perform
        // the promised confirmation now rather than exiting before the retry.
        if try !locked() { return true }
      }
      pause()
    } while now() < deadline
    return false
  }

  public func key(_ key: String) throws {
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

  public func type(_ text: String, append: Bool = true, submit: Bool = false) throws {
    guard try !locked() else { throw PhoneError("Phone is locked; use unlock") }
    let keyboard = try hid()
    try keyboard.prepareKeyboard()
    if !text.isEmpty {
      do {
        try nativeConnection().setPasteboard(text)
      } catch {
        _ = try Command.device(["device", "pasteboard", "copy"], id: info.id, input: Data(text.utf8))
      }
    }
    if !append { try keyboard.keys([0xe3, 0x04]); Thread.sleep(forTimeInterval: 0.1) }
    if !text.isEmpty { try keyboard.keys([0xe3, 0x19]) }
    else if !append { try keyboard.keys([0x2a]) }
    if submit { Thread.sleep(forTimeInterval: 0.1); try keyboard.keys([0x28]) }
  }

  func apps(maxAge: Double = 30) throws -> [[String: Any]] {
    try CoreConnectionFailure.retryRead {
      // Validate the connection before consulting its catalog cache.
      let connection = try nativeConnection()
      let now = ProcessInfo.processInfo.systemUptime
      if let cachedApps, now - appsAt < maxAge { return cachedApps }
      let result = try connection.stream("com.apple.coredevice.appservice",
        feature:"com.apple.coredevice.feature.streamapplist",input:[
          "includeAppClips":false, "includeRemovableApps":true, "includeHiddenApps":false,
          "includeInternalApps":false, "includeDefaultApps":true,
          "requireContainerAccess":false, "includeAppGroupIdentifiers":false, "includeContainerPaths":false,
        ])
      guard let apps=result as? [[String:Any]] else {
        throw PhoneError("Invalid application catalog")
      }
      cachedApps = apps
      appsAt = ProcessInfo.processInfo.systemUptime
      return apps
    }
  }

  static func matchApp(_ name: String, in catalog: [[String: Any]]) throws -> [String: Any]? {
    let exact = catalog.filter { $0["bundleIdentifier"] as? String == name }
    func normalized(_ value:String) -> String {
      let visible=value.unicodeScalars.filter { !$0.properties.isBidiControl && $0.value != 0xfeff }
      return String(String.UnicodeScalarView(visible)).precomposedStringWithCanonicalMapping
        .trimmingCharacters(in:.whitespacesAndNewlines).lowercased()
    }
    let wanted=normalized(name)
    guard !wanted.isEmpty else { return nil }
    let matches = exact.isEmpty ? catalog.filter {
      if let id = $0["bundleIdentifier"] as? String, id.lowercased() == wanted { return true }
      guard let displayName=$0["name"] as? String else { return false }
      return normalized(displayName)==wanted
    } : exact
    if matches.isEmpty { return nil }
    guard matches.count == 1 else {
      throw PhoneError("App name is ambiguous; use a bundle identifier from apps")
    }
    return matches[0]
  }

  func resolveApp(_ name: String) throws -> [String: Any] {
    if let found = try Self.matchApp(name, in: try apps(maxAge: 30)) { return found }
    if ProcessInfo.processInfo.systemUptime - appsAt >= 5,
       let found = try Self.matchApp(name, in: try apps(maxAge: 0)) { return found }
    throw PhoneError("App name is missing or ambiguous; use a bundle identifier from apps")
  }

  func appID(_ name: String) throws -> String {
    guard let id = try resolveApp(name)["bundleIdentifier"] as? String else {
      throw PhoneError("App bundle identifier is unavailable")
    }
    return id
  }

  private func launchBundle(_ bundle: String, payloadURL: String? = nil) throws {
    // Wire format from go-ios ios/appservice/appservice.go (MIT; see NOTICE).
    let platform = try PropertyListSerialization.data(
      fromPropertyList: [String: Any](), format: .binary, options: 0)
    var options: [String: Any] = [
      "arguments": [Any](), "environmentVariables": [String: Any](),
      "platformSpecificOptions": platform,
      "standardIOUsesPseudoterminals": true, "startStopped": false,
      "terminateExisting": false, "user": ["active": true],
      "workingDirectory": NSNull(),
    ]
    if let payloadURL { options["payloadURL"] = ["relative": payloadURL] }
    let result = try nativeConnection().invoke("com.apple.coredevice.appservice",
      feature: "com.apple.coredevice.feature.launchapplication", input: [
        "applicationSpecifier": ["bundleIdentifier": ["_0": bundle]],
        "options": options,
        "standardIOIdentifiers": [String: Any](),
      ])
    guard let output = CoreConnection.object(result) as? [String: Any],
          let process = output["processToken"] as? [String: Any],
          let pid = process["processIdentifier"] as? NSNumber, pid.intValue > 0 else {
      throw PhoneError("App launch did not return a process identifier")
    }
  }

  func launchApp(_ name: String) throws {
    try launchBundle(try appID(name))
  }

  static func canonicalDevicePath(_ raw: String) -> String? {
    let url = raw.hasPrefix("/") ? URL(fileURLWithPath: raw) : URL(string: raw)
    guard var path = url?.standardized.path, !path.isEmpty else { return nil }
    while path.count > 1 && path.hasSuffix("/") { path.removeLast() }
    if path.hasPrefix("/private/") { path.removeFirst("/private".count) }
    return path
  }

  func stopApp(_ name: String) throws {
    let app = try resolveApp(name)
    guard let rawBundle = app["path"] as? String,
          let bundlePath = Self.canonicalDevicePath(rawBundle) else {
      throw PhoneError("The app bundle path is unavailable")
    }
    let connection=try nativeConnection()
    let processes=try connection.stream("com.apple.coredevice.appservice",
      feature:"com.apple.coredevice.feature.streamprocesslist",input:[:])
    for case let process as [String:Any] in processes {
      guard let executable = process["executableURL"] as? [String:Any],
            let relative = executable["relative"] as? String,
            let execPath = Self.canonicalDevicePath(relative),
            let slash = execPath.lastIndex(of: "/"), slash > execPath.startIndex,
            String(execPath[..<slash]) == bundlePath,
            let pid = process["processIdentifier"] as? NSNumber, pid.intValue > 0 else { continue }
      _ = try connection.invoke("com.apple.coredevice.appservice",
        feature:"com.apple.coredevice.feature.sendsignaltoprocess",input:[
          "process":["processIdentifier":pid.int64Value], "signal":Int64(SIGKILL),
        ])
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
          // A port that does not answer as accessibility is stale.
          do { axSession = try DTXSession(socket:socket) }
          catch { connection.invalidateDirectory(); throw error }
          connection.confirmDirectory()
        }
        return try Screen(responses:axSession!.tree(preferPush: preferPush),scale:display()["scale"] as? Double ?? 3,locked:locked())
      } catch {
        axSession?.close()
        axSession=nil
        guard attempt==0 else { throw error }
        if let failure = error as? AccessibilityFailure {
          // An app launch or transition can reject a read (errors 8 and 17
          // have been seen). Let the screen settle and read once more from the
          // new foreground app. Only observation is repeated, never input.
          if case .rejected = failure { Thread.sleep(forTimeInterval: 0.4) }
          if case .disconnected = failure { axConnection?.invalidateDirectory() }
        } else if let failure = error as? PhoneError,
                  failure.localizedDescription == "No foreground accessibility application"
                  || failure.localizedDescription == "Accessibility did not provide screen bounds" {
          Thread.sleep(forTimeInterval: 0.4)
        } else {
          throw error
        }
      }
    }
    throw PhoneError("Accessibility unavailable")
  }

  static func validatedLink(_ raw: String) throws -> String {
    let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !trimmed.isEmpty,
          !trimmed.unicodeScalars.contains(where: { $0.properties.isWhitespace || ($0.value < 0x20 || $0.value == 0x7f) }),
          let parsed = URL(string: trimmed), let scheme = parsed.scheme?.lowercased(), !scheme.isEmpty else {
      throw PhoneError("URL requires a valid scheme")
    }
    if (scheme == "http" || scheme == "https"), (parsed.host ?? "").isEmpty {
      throw PhoneError("Web URL requires a host")
    }
    return trimmed
  }

  func openURL(_ url: String, app: String? = nil) throws {
    let valid = try Self.validatedLink(url)
    let handler = try app.map { try appID($0) } ?? "com.apple.mobilesafari"
    try launchBundle(handler, payloadURL: valid)
  }
}
