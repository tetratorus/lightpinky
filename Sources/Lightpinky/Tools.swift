import Foundation
import CoreGraphics

final class Tools {
  private var phones: [String: Phone] = [:]
  private var defaultPhone: Phone?
  func shutdown() { defaultPhone = nil; phones.removeAll() }
  /// The stream makes the phone encode its screen continuously, so it
  /// should not outlive the commands that needed it.
  func endDisplaySessions() { phones.values.forEach { $0.endDisplaySession() } }
  static let descriptions: [String: String] = [
    "devices": "List paired iPhones known to macOS CoreDevice.",
    "download_disk_image": "Prepare Apple's developer services using the DDI installed by Xcode.",
    "power": "Read lock and display state; wake:true explicitly wakes the screen.",
    "unlock": "Wake and unlock using IPHONE_UNLOCK_CODE from the server environment. Never retries passcode input.",
    "lock": "Lock the iPhone with one side-button press and verify locked state. Already locked and asleep is a no-op.",
    "screenshot": "JPEG screenshot. Coordinates use full screen pixels; max_size returns a scale factor. video:true takes the frame from the display video stream instead of a full PNG transfer. png:true returns lossless PNG instead of JPEG.",
    "elements": "Read the foreground application's native accessibility tree. json:true returns the full structure.",
    "tap": "Tap or long-press in screenshot pixels, or find an element by label or identifier. Use role or zero-based index to disambiguate.",
    "swipe": "Swipe between screenshot pixel coordinates.",
    "type": "Type through the device pasteboard. Replaces focused field text unless append:true.",
    "key": "Press home, back, enter, tab, delete, forward_delete, escape, dpad_up/down/left/right, page_up/down.",
    "apps": "List installed apps and bundle identifiers.",
    "launch": "Open an app by exact name or bundle identifier.",
    "stop_app": "Quit an app by exact name or bundle identifier.",
    "open_link": "Open a URL through Safari, or pass app to deliver it to a specific installed app.",
  ]

  static func schema() -> [[String: Any]] {
    descriptions.keys.sorted().map { name in
      var properties: [String: Any] = [:]
      var required: [String] = []
      func field(_ key: String, _ type: String, required must: Bool = false) {
        properties[key] = ["type": type]
        if must { required.append(key) }
      }
      if name != "devices" { field("device", "string") }
      if ["tap","swipe","type","key","launch","stop_app","open_link"].contains(name) { field("no_settle","boolean") }
      switch name {
      case "power": field("wake","boolean")
      case "screenshot": field("max_size","integer"); field("video","boolean"); field("png","boolean")
      case "elements": field("json","boolean")
      case "tap":
        field("x","number"); field("y","number"); field("long_press","boolean")
        field("find","string"); field("role","string"); field("index","integer")
      case "swipe":
        for k in ["x1","y1","x2","y2"] { field(k,"number",required:true) }
        field("duration_ms","number")
      case "type": field("text","string",required:true); field("append","boolean"); field("submit","boolean")
      case "key": field("key","string",required:true)
      case "launch","stop_app": field("app","string",required:true)
      case "open_link": field("url","string",required:true); field("app","string")
      default: break
      }
      return ["name": name, "description": descriptions[name]!,
        "inputSchema": ["type":"object", "properties": properties, "required": required, "additionalProperties":false]]
    }
  }

  func phone(_ requested: String?) throws -> Phone {
    if let requested, let phone = phones[requested] { return phone }
    if requested == nil, let defaultPhone { return defaultPhone }
    let available = try PhoneInfo.discover()
    let matching = requested.map { key in available.filter { $0.id == key || $0.udid == key || $0.name == key } } ?? available
    guard matching.count == 1 else { throw PhoneError("Choose a device from devices (found \(matching.count) matching iPhones)") }
    let info = matching[0]
    if let phone = phones[info.id] {
      if requested == nil { defaultPhone = phone }
      return phone
    }
    let phone = Phone(info)
    phones[info.id] = phone; phones[info.udid] = phone
    if requested == nil { defaultPhone = phone }
    return phone
  }

  static func text(_ value: Any) throws -> [String: Any] {
    let text: String
    if let string = value as? String { text = string }
    else { text = String(decoding: try JSONSerialization.data(withJSONObject:value,options:[.prettyPrinted,.sortedKeys,.fragmentsAllowed]),as:UTF8.self) }
    return ["content": [["type":"text", "text":text]]]
  }

  func call(_ name: String, _ args: [String: Any]) throws -> [String: Any] {
    guard let schema = Self.schema().first(where: { $0["name"] as? String == name }),
          let inputSchema = schema["inputSchema"] as? [String: Any],
          let properties = inputSchema["properties"] as? [String: Any] else { throw PhoneError("Unknown tool \(name)") }
    guard args.keys.allSatisfy({ properties[$0] != nil }) else { throw PhoneError("Unexpected argument; discover arguments with tools/list") }
    for (key,value) in args {
      let type=(properties[key] as? [String:Any])?["type"] as? String
      switch type {
      case "string": guard value is String else { throw PhoneError("\(key) must be a string") }
      case "boolean":
        guard let value=value as? NSNumber, CFGetTypeID(value)==CFBooleanGetTypeID() else { throw PhoneError("\(key) must be a boolean") }
      case "number","integer":
        guard let value=value as? NSNumber, CFGetTypeID(value) != CFBooleanGetTypeID(), value.doubleValue.isFinite,
              type != "integer" || value.doubleValue.rounded()==value.doubleValue else { throw PhoneError("\(key) must be a finite \(type!)") }
      default: break
      }
    }
    for required in inputSchema["required"] as? [String] ?? [] where args[required] == nil {
      throw PhoneError("Missing argument \(required)")
    }
    func number(_ key: String, _ fallback: Double = 0) throws -> Double {
      guard let value = args[key] else { return fallback }
      guard let n = value as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), n.doubleValue.isFinite else { throw PhoneError("\(key) must be a finite number") }
      return n.doubleValue
    }
    func string(_ key: String) throws -> String {
      guard let value = args[key] as? String else { throw PhoneError("\(key) must be a string") }
      return value
    }
    if name == "devices" {
      let available = try PhoneInfo.discover()
      defaultPhone = available.count == 1 ? phones[available[0].id] : nil
      return try Self.text(available.map(\.json))
    }
    let phone = try phone(args["device"] as? String)
    switch name {
    case "power":
      if args["wake"] as? Bool == true { try phone.wake() }
      return try Self.text(phone.power())
    case "unlock": try phone.unlock()
    case "lock": try phone.lock()
    case "download_disk_image": _ = try phone.hid()
    case "screenshot":
      let size = try number("max_size")
      guard size >= 0 && size <= 10000 else { throw PhoneError("max_size must be 0-10000") }
      var shot: CGImage?
      if args["video"] as? Bool == true {
        shot = try phone.videoFrame(wait:5)
        if shot == nil { throw PhoneError("No current frame from the display video stream") }
      }
      let (image,note) = try Phone.jpeg(shot ?? phone.screenshot(),maxSize:Int(size),lossless:args["png"] as? Bool == true)
      return ["content":[image,["type":"text","text":note]]]
    case "elements":
      let screen=try phone.elements()
      return try Self.text(args["json"] as? Bool == true ? screen.json : screen.text)
    case "tap":
      let x:Double,y:Double
      if let query=args["find"] as? String, !query.isEmpty {
        let index=try args["index"].map { _ in try number("index") }
        guard index == nil || (index!>=0 && index!<=400 && index!.rounded()==index!) else { throw PhoneError("index must be an integer from 0 to 400") }
        (x,y)=try phone.elements().find(query,role:args["role"] as? String,index:index.map(Int.init))
      } else {
        guard args["x"] != nil,args["y"] != nil else { throw PhoneError("Tap requires x/y or find") }
        x=try number("x"); y=try number("y")
      }
      try phone.gesture(x1:x,y1:y,x2:x,y2:y,duration:args["long_press"] as? Bool == true ? 0.8 : 0.06)
    case "swipe":
      try phone.gesture(x1:number("x1"),y1:number("y1"),x2:number("x2"),y2:number("y2"),duration:number("duration_ms",300)/1000)
    case "type": try phone.type(string("text"),append:args["append"] as? Bool == true,submit:args["submit"] as? Bool == true)
    case "key": try phone.key(string("key"))
    case "apps": return try Self.text(phone.apps())
    case "launch":
      try phone.launchApp(string("app"))
    case "stop_app":
      try phone.stopApp(string("app"))
    case "open_link": try phone.openURL(string("url"),app:args["app"] as? String)
    default: throw PhoneError("Unknown tool")
    }
    if args["no_settle"] as? Bool != true { Thread.sleep(forTimeInterval:0.35) }
    return try Self.text("ok")
  }
}
