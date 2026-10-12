#!/bin/bash
# Builds dist/phoneOnDesk-mac.zip: one app for Apple Silicon and Intel Macs.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
rm -rf dist/mac && mkdir -p dist/mac

for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "dist/mac/phoneondesk-$arch" .
done

APP="dist/mac/phoneOnDesk.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
lipo -create -output "$APP/Contents/MacOS/phoneondesk" dist/mac/phoneondesk-arm64 dist/mac/phoneondesk-amd64
cp assets/icon.icns "$APP/Contents/Resources/icon.icns"
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>phoneOnDesk</string>
  <key>CFBundleDisplayName</key><string>phoneOnDesk</string>
  <key>CFBundleIdentifier</key><string>app.phoneondesk</string>
  <key>CFBundleExecutable</key><string>phoneondesk</string>
  <key>CFBundleIconFile</key><string>icon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${VERSION#v}</string>
  <key>CFBundleVersion</key><string>${VERSION#v}</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <!-- No window of its own: it lives in a browser tab and quits when that closes. -->
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST

# Ad-hoc signature: Apple Silicon won't run unsigned code. Not notarised, so the first open needs
# System Settings → Privacy & Security → Open Anyway.
codesign --force --deep --sign - "$APP"
ditto -c -k --keepParent "$APP" dist/phoneOnDesk-mac.zip
echo "Built dist/phoneOnDesk-mac.zip ($VERSION)"
