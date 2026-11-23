// swift-tools-version:6.0
import PackageDescription

// The macOS app around Connect: a menu-bar program that runs the Go core
// (cmd/nvto-connect) and opens a rented Mac's remote desktop or a terminal
// on it, from the menu or from a link on the NVTO website.
let package = Package(
    name: "NVTOConnect",
    platforms: [.macOS(.v13)],
    dependencies: [
        // Sparkle updates the app, as it does the NVTO Node app.
        .package(url: "https://github.com/sparkle-project/Sparkle", from: "2.6.0"),
    ],
    targets: [
        // Everything that is not user interface: running the core, reading
        // what it reports, and handing a connection to macOS's own apps.
        .target(name: "ConnectKit"),
        .executableTarget(
            name: "NVTOConnect",
            dependencies: ["ConnectKit", .product(name: "Sparkle", package: "Sparkle")],
            // Sparkle.framework is copied into the app by scripts/bundle.sh.
            linkerSettings: [.unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"])]),
        .testTarget(name: "ConnectKitTests", dependencies: ["ConnectKit"]),
    ],
    swiftLanguageModes: [.v5]
)
