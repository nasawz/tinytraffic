// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "TinyTraffic",
    platforms: [.iOS(.v15), .macOS(.v12)],
    products: [.library(name: "TinyTraffic", targets: ["TinyTraffic"])],
    targets: [
        .target(name: "TinyTraffic"),
        .testTarget(name: "TinyTrafficTests", dependencies: ["TinyTraffic"]),
    ]
)
