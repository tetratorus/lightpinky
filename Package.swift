// swift-tools-version: 5.9
import PackageDescription

let package = Package(
  name: "lightpinky",
  platforms: [.macOS(.v14)],
  products: [.executable(name: "lightpinky", targets: ["Lightpinky"])],
  targets: [
    .target(name: "AppleBridge", publicHeadersPath: "include",
            cSettings: [.unsafeFlags(["-fobjc-arc"])], linkerSettings: [.linkedLibrary("z")]),
    .executableTarget(name: "Lightpinky", dependencies: ["AppleBridge"],
      linkerSettings: [
        .unsafeFlags(["-F/Library/Developer/PrivateFrameworks", "-framework", "CoreDevice",
                      "-Xlinker", "-rpath", "-Xlinker", "/Library/Developer/PrivateFrameworks"]),
      ]),
    .testTarget(name: "LightpinkyTests", dependencies: ["Lightpinky"]),
  ]
)
