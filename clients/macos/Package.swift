// swift-tools-version: 5.9

import PackageDescription

let package = Package(
    name: "LexinetMenuBar",
    platforms: [
        .macOS(.v13)
    ],
    products: [
        .executable(name: "LexinetMenuBar", targets: ["LexinetMenuBar"]),
        .library(name: "LexinetClient", targets: ["LexinetClient"])
    ],
    targets: [
        .target(name: "LexinetClient"),
        .executableTarget(
            name: "LexinetMenuBar",
            dependencies: ["LexinetClient"]
        ),
        .testTarget(
            name: "LexinetClientTests",
            dependencies: ["LexinetClient"],
            resources: [.copy("Fixtures")]
        )
    ]
)
