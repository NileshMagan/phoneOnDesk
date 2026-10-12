#!/bin/bash
# Builds "phoneOnDesk.app" into ~/Applications. Re-run after moving this folder or changing Node.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
NODE="$(command -v node)"
APP="$HOME/Applications/phoneOnDesk.app"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

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
  <key>CFBundleVersion</key><string>1</string>
</dict>
</plist>
PLIST

cat > "$APP/Contents/MacOS/phoneondesk" <<LAUNCHER
#!/bin/bash
# Start the server unless it's already up, then open the viewer.
LOG="\$HOME/Library/Logs/phoneondesk.log"
if ! curl -sf http://localhost:8080/status >/dev/null; then
  SCREEN_APP="\$(cd "\$(dirname "\$0")/../Helpers" && pwd)/Phone Screen.app" \\
    nohup "$NODE" "$DIR/server.mjs" --app >>"\$LOG" 2>&1 &
  for _ in \$(seq 1 50); do
    curl -sf http://localhost:8080/status >/dev/null && break
    sleep 0.1
  done
fi
open http://localhost:8080/
LAUNCHER
chmod +x "$APP/Contents/MacOS/phoneondesk"

if [ -f "$DIR/icon.icns" ]; then cp "$DIR/icon.icns" "$APP/Contents/Resources/icon.icns"; fi

# scrcpy started from a background process opens its window hidden. Wrapping it in its own
# app bundle lets macOS launch it as a normal foreground app with a Dock icon.
SCREEN="$APP/Contents/Helpers/Phone Screen.app"
mkdir -p "$SCREEN/Contents/MacOS" "$SCREEN/Contents/Resources"
cat > "$SCREEN/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Phone Screen</string>
  <key>CFBundleIdentifier</key><string>app.phoneondesk.screen</string>
  <key>CFBundleExecutable</key><string>phone-screen</string>
  <key>CFBundleIconFile</key><string>icon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleVersion</key><string>1</string>
</dict>
</plist>
PLIST
cat > "$SCREEN/Contents/MacOS/phone-screen" <<'SCRCPY'
#!/bin/bash
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
exec scrcpy "$@" >>"$HOME/Library/Logs/phoneondesk.log" 2>&1
SCRCPY
chmod +x "$SCREEN/Contents/MacOS/phone-screen"
if [ -f "$DIR/icon.icns" ]; then cp "$DIR/icon.icns" "$SCREEN/Contents/Resources/icon.icns"; fi
touch "$APP"
echo "Built: $APP"
