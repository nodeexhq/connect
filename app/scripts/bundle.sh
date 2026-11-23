#!/bin/sh
# Builds "NodeEx Connect.app": the Swift menu-bar app with the Go core inside
# it, for both Apple silicon and Intel Macs, in app/dist/.
#
#   app/scripts/bundle.sh                          ad-hoc signature, for this Mac only
#   SIGN_IDENTITY=<SHA-1> app/scripts/bundle.sh    Developer ID signature
set -eu
APP_DIR=$(cd "$(dirname "$0")/.." && pwd)
REPO=$(cd "$APP_DIR/.." && pwd)
VERSION=${VERSION:-$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo dev)}
IDENTITY=${SIGN_IDENTITY:--}
# Sparkle compares build numbers, so the build is a plain count of commits.
BUILD=${BUILD:-$(git -C "$REPO" rev-list --count HEAD 2>/dev/null || echo 1)}
# Where the app looks for updates. Empty builds an app that never updates.
FEED_URL=${FEED_URL-https://raw.githubusercontent.com/nodeexhq/connect/main/app/appcast.xml}
DIST="$APP_DIR/dist"
BUNDLE="$DIST/NodeEx Connect.app"

(cd "$APP_DIR" && swift build -c release --arch arm64 --arch x86_64)
BIN=$(cd "$APP_DIR" && swift build -c release --arch arm64 --arch x86_64 --show-bin-path)

mkdir -p "$DIST"
[ ! -d "$BUNDLE" ] || rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/Contents/MacOS" "$BUNDLE/Contents/Helpers" "$BUNDLE/Contents/Resources" "$BUNDLE/Contents/Frameworks"
cp "$APP_DIR/Resources/AppIcon.icns" "$BUNDLE/Contents/Resources/AppIcon.icns"
cp -R "$APP_DIR/.build/artifacts/sparkle/Sparkle/Sparkle.xcframework/macos-arm64_x86_64/Sparkle.framework" "$BUNDLE/Contents/Frameworks/"
cp "$BIN/NodeExConnect" "$BUNDLE/Contents/MacOS/NodeExConnect"
TMP=$(mktemp -d)
"$REPO/scripts/build-guest.sh"
for arch in arm64 amd64; do
  (cd "$REPO" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -tags guestbin -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$TMP/nodeex-connect-$arch" ./cmd/nodeex-connect)
done
lipo -create -output "$BUNDLE/Contents/Helpers/nodeex-connect" "$TMP/nodeex-connect-arm64" "$TMP/nodeex-connect-amd64"
rm -rf "$TMP"

UPDATE_KEYS=""
if [ -n "$FEED_URL" ]; then
  UPDATE_KEYS="	<key>SUFeedURL</key><string>$FEED_URL</string>
	<key>SUPublicEDKey</key><string>$(cat "$APP_DIR/sparkle_public_key")</string>
	<key>SUEnableAutomaticChecks</key><true/>
	<key>SUAutomaticallyUpdate</key><true/>
	<key>SUScheduledCheckInterval</key><integer>21600</integer>"
fi

cat > "$BUNDLE/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>NVTO Connect</string>
	<key>CFBundleDisplayName</key><string>NVTO Connect</string>
	<key>CFBundleIdentifier</key><string>com.nodeex.connect</string>
	<key>CFBundleExecutable</key><string>NodeExConnect</string>
	<key>CFBundleIconFile</key><string>AppIcon</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>${VERSION#v}</string>
	<key>CFBundleVersion</key><string>${BUILD}</string>
	<key>CFBundleURLTypes</key>
	<array>
		<dict>
			<key>CFBundleURLName</key><string>com.nodeex.connect</string>
			<key>CFBundleURLSchemes</key><array><string>nodeex-connect</string></array>
		</dict>
	</array>
${UPDATE_KEYS}
	<key>LSMinimumSystemVersion</key><string>13.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

sign() {
  if [ "$IDENTITY" = "-" ]; then codesign --force -s - "$@"; else codesign --force --options runtime --timestamp -s "$IDENTITY" "$@"; fi
}
SPARKLE="$BUNDLE/Contents/Frameworks/Sparkle.framework"
sign "$SPARKLE/Versions/B/XPCServices/Installer.xpc"
sign --preserve-metadata=entitlements "$SPARKLE/Versions/B/XPCServices/Downloader.xpc"
sign "$SPARKLE/Versions/B/Autoupdate"
sign "$SPARKLE/Versions/B/Updater.app"
sign "$SPARKLE"
sign "$BUNDLE/Contents/Helpers/nodeex-connect"
sign "$BUNDLE"
codesign --verify --strict "$BUNDLE"
echo "built $BUNDLE ($VERSION, build $BUILD)"
