import Foundation

struct Screen {
  let json: [String:Any]
  let elements: [[String:Any]]

  init(responses:[[String:Any]],scale:Double,locked:Bool) throws {
    let attributes=responses.dropFirst().compactMap { $0["result"] as? [String:Any] }
    guard let root=attributes.first, let frame=root["21"] as? [String:Double] else {
      throw PhoneError("Accessibility did not provide screen bounds")
    }
    let width=Int(((frame["width"] ?? 0)*scale).rounded())
    let height=Int(((frame["height"] ?? 0)*scale).rounded())
    guard width>0, height>0 else { throw PhoneError("Invalid accessibility screen size") }
    var output:[[String:Any]]=[]
    var keyboard=false
    for data in attributes.dropFirst() {
      let type=data["61"] as? Int ?? 0
      keyboard = keyboard || type==19 || type==20
      if (data["32"] as? NSNumber)?.boolValue == false { continue }
      guard let frame=data["21"] as? [String:Double] else { continue }
      let x=Int(((frame["x"] ?? 0)*scale).rounded()), y=Int(((frame["y"] ?? 0)*scale).rounded())
      let w=Int(((frame["width"] ?? 0)*scale).rounded()), h=Int(((frame["height"] ?? 0)*scale).rounded())
      guard w>0,h>0,x<width,y<height,x+w>0,y+h>0 else { continue }
      let identifier=data["25"] as? String ?? ""
      let name=(data["33"] as? String ?? "").trimmingCharacters(in:.whitespacesAndNewlines)
      let rawValue=((data["64"] as? String).flatMap { $0.isEmpty ? nil : $0 }
        ?? (data["53"] as? String) ?? (data["53"] as? NSNumber)?.stringValue ?? "")
        .trimmingCharacters(in:.whitespacesAndNewlines)
      let placeholder=(data["42"] as? String ?? "").trimmingCharacters(in:.whitespacesAndNewlines)
      let axRole=data["45"] as? Int ?? 0
      let traits=(data["77"] as? NSNumber)?.uint64Value ?? 0
      var role:String
      switch type {
      case 9,14,16,17,20,44,54,59,60,75: role="button"
      case 45,49,50,52: role="input"
      case 48: role="text"
      case 43: role="image"
      case 26,32,46: role="list"
      case 40,41: role="switch"
      case 10,12: role="checkbox"
      case 22,80: role="tab"
      case 42: role="link"
      default:
        role=traits & 1 != 0 ? "button" : traits & 2 != 0 ? "link" : traits & 64 != 0 ? "text" : "other"
        // A first pushed tree lacks AutomationType (61); AXRole (45) carries it.
        switch axRole {
        case 3: role="switch"
        case 15: role="input"
        case 7 where role=="other": role="image"
        case 2 where role=="other": role="button"
        case 6 where role=="other", 14 where role=="other": role="text"
        default: break
        }
      }
      if axRole==9 && (role=="button" || role=="other") { role="tab" }
      let suppressValue = role=="switch" || role=="checkbox" || (role=="tab" && ["0","1"].contains(rawValue))
      let value = suppressValue ? "" : rawValue
      var label = name
      var detail = value
      if label.isEmpty { label = detail; detail = "" }
      if label.isEmpty { label = placeholder }
      if !detail.isEmpty && detail != label { label += ", \(detail)" }
      guard !label.isEmpty || !identifier.isEmpty || role != "other" else { continue }
      var states:[String]=[]
      if (data["27"] as? NSNumber)?.boolValue == false { states.append("disabled") }
      if (data["28"] as? NSNumber)?.boolValue == true { states.append("focused") }
      if (data["30"] as? NSNumber)?.boolValue == true { states.append("selected") }
      if role=="input" { states.append("editable") }
      if type==50 || (role=="input" && traits & (1 << 24) != 0) { states.append("password") }
      if role=="list" { states.append("scrollable") }
      if ["switch","checkbox"].contains(role), ["1","on","开"].contains(rawValue.lowercased()) { states.append("checked") }
      output.append(["Ref":output.count+1,"Role":role,"Label":label,"ID":identifier,
        "Bounds":["X":x,"Y":y,"W":w,"H":h],"States":states,"Source":"tree"])
    }
    elements=output
    json=["Width":width,"Height":height,"Locked":locked,"Keyboard":keyboard,
      "Orientation":width>height ? "landscape" : "portrait",
      "App":["ID":root["25"] as? String ?? "","Name":(root["33"] as? String ?? "").trimmingCharacters(in:.whitespacesAndNewlines)],
      "Elements":output,"Events":[],"CapturedAt":ISO8601DateFormatter().string(from:Date())]
  }

  var text:String {
    var header="screen \(json["Width"]!)x\(json["Height"]!), \(json["Orientation"]!)"
    if json["Locked"] as? Bool == true { header+=", locked" }
    if let app=json["App"] as? [String:String], let name=app["Name"], !name.isEmpty { header+=", app: \(name)" }
    header+=", keyboard \(json["Keyboard"] as? Bool == true ? "shown" : "hidden")"
    return ([header]+elements.map { element in
      let rect=element["Bounds"] as! [String:Int]
      let x=rect["X"]!,y=rect["Y"]!,w=rect["W"]!,h=rect["H"]!
      var line="\(element["Role"]!) \(String(reflecting:element["Label"] as! String))"
      if let id=element["ID"] as? String, !id.isEmpty { line+=" id=\(String(reflecting:id))" }
      line+=" @\(x+w/2),\(y+h/2) [\(x),\(y) \(w)x\(h)]"
      for state in element["States"] as! [String] { line+=" \(state)" }
      return line
    }).joined(separator:"\n")
  }

  func find(_ query:String,role:String?,index:Int?) throws -> (Double,Double) {
    func clean(_ text:String) -> String {
      let visible=text.unicodeScalars.filter { !$0.properties.isBidiControl && $0.value != 0xfeff }
      return String(String.UnicodeScalarView(visible)).precomposedStringWithCanonicalMapping
        .trimmingCharacters(in:.whitespacesAndNewlines)
    }
    let needle=clean(query)
    func matches(_ element:[String:Any],exact:Bool) -> Bool {
      if let role, element["Role"] as? String != role { return false }
      return ["Label","ID"].contains { key in
        let value=clean(element[key] as? String ?? "")
        return exact ? value.caseInsensitiveCompare(needle) == .orderedSame : value.localizedCaseInsensitiveContains(needle)
      }
    }
    let exact=elements.filter { matches($0,exact:true) }
    let candidates=exact.isEmpty ? elements.filter { matches($0,exact:false) } : exact
    let selected=index ?? 0
    guard !candidates.isEmpty else {
      let app = (json["App"] as? [String: Any])?["Name"] as? String ?? "unknown app"
      let state = json["Locked"] as? Bool == true ? ", phone locked" : ""
      throw PhoneError("No element matches \(query) in \(app)\(state) (\(elements.count) elements read)")
    }
    guard index != nil || candidates.count==1 else { throw PhoneError("Multiple elements match; use role or zero-based index") }
    guard selected>=0,selected<candidates.count else { throw PhoneError("Element index out of range") }
    let rect=candidates[selected]["Bounds"] as! [String:Int]
    return (Double(rect["X"]!)+Double(rect["W"]!)/2,Double(rect["Y"]!)+Double(rect["H"]!)/2)
  }
}
