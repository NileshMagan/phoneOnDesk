#!/bin/bash
# Builds dist/phoneOnDesk-linux-{x64,arm64}.AppImage. Needs appimagetool on PATH (CI downloads it).
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist

for pair in "amd64 x86_64 x64" "arm64 aarch64 arm64"; do
  set -- $pair
  goarch=$1 arch=$2 name=$3
  DIR="dist/linux-$name/phoneOnDesk.AppDir"
  rm -rf "dist/linux-$name" && mkdir -p "$DIR/usr/bin"
  CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -ldflags "-s -w" -o "$DIR/usr/bin/phoneondesk" .
  cp assets/icon.png "$DIR/phoneondesk.png"
  cat > "$DIR/phoneondesk.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=phoneOnDesk
Comment=Your phone's camera and screen in your browser
Exec=phoneondesk
Icon=phoneondesk
Categories=Utility;
Terminal=false
DESKTOP
  cat > "$DIR/AppRun" <<'APPRUN'
#!/bin/sh
HERE="$(dirname "$(readlink -f "$0")")"
exec "$HERE/usr/bin/phoneondesk" --app "$@"
APPRUN
  chmod +x "$DIR/AppRun"
  ARCH=$arch appimagetool --no-appstream "$DIR" "dist/phoneOnDesk-linux-$name.AppImage"
done
echo "Built Linux AppImages"
