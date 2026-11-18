// swift-tools-version: 5.9
import PackageDescription

let package = Package(
  name: "lightpinky",
  platforms: [.macOS(.v14)],
  products: [
    .executable(name: "lightpinky", targets: ["Lightpinky"]),
    .library(name: "LightpinkyCore", targets: ["LightpinkyCore"]),
  ],
  targets: [
    .target(name: "AppleBridge", publicHeadersPath: "include",
            cSettings: [.unsafeFlags(["-fobjc-arc"])], linkerSettings: [.linkedLibrary("z")]),
    .target(name: "LightpinkyCore", dependencies: ["AppleBridge"], path: "Sources/Lightpinky",
      linkerSettings: [
        .unsafeFlags(["-F/Library/Developer/PrivateFrameworks", "-framework", "CoreDevice",
                      "-Xlinker", "-rpath", "-Xlinker", "/Library/Developer/PrivateFrameworks"]),
      ]),
    .executableTarget(name: "Lightpinky", dependencies: ["LightpinkyCore"], path: "Sources/CLI"),
    .testTarget(name: "LightpinkyTests", dependencies: ["LightpinkyCore"]),
  ]
)
