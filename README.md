# phoneOnDesk

Use the phone on your desk from your computer's browser:

- **Camera.** Watch the phone's camera (a 3D printer, a doorway, a pot on the stove) with its battery level.
- **Phone screen.** See it and use it in the page: click to tap, drag to swipe, scroll, type. Android only.
- **Clipboard.** Send text both ways.

It runs on your computer and talks to the phone over your own Wi‑Fi. Nothing goes through anyone's servers.

## Download

- **Mac** (Apple Silicon and Intel): [phoneOnDesk-mac.zip](https://github.com/NileshMagan/phoneOnDesk/releases/latest/download/phoneOnDesk-mac.zip)
- **Linux** (x64): [phoneOnDesk-linux-x64.AppImage](https://github.com/NileshMagan/phoneOnDesk/releases/latest/download/phoneOnDesk-linux-x64.AppImage)
- **Linux** (arm64): [phoneOnDesk-linux-arm64.AppImage](https://github.com/NileshMagan/phoneOnDesk/releases/latest/download/phoneOnDesk-linux-arm64.AppImage)

**Mac:** unzip it and move phoneOnDesk to Applications. It isn't notarised by Apple yet. The first time you open it, macOS refuses. Go to **System Settings → Privacy & Security** and click **Open Anyway**.

**Linux:** mark the AppImage as executable (`chmod +x`, or Properties → Allow executing), then open it.

## Use

1. Open phoneOnDesk. Your browser shows its page with a QR code.
2. On the phone, scan the code (it carries a key, so only your phone gets in), continue past the certificate warning once (it's your computer's own certificate), allow the camera, and tap **Start**.
3. For the phone's screen, click **Phone screen** and follow the one-time pairing steps. The first time, it offers to download Android's own tools (adb, from Google).
4. Close the tab when you're done. phoneOnDesk quits about 20 seconds later.

Pages on other websites can show phoneOnDesk too, but only websites on its allow list (`https://ovelle.me` by default). The first time, the browser has to accept phoneOnDesk's certificate at `https://localhost:8443/connected`.

## How it works

- A single Go program with no dependencies. The pages in `web/` are built into it.
- **Camera:** the phone's page (`web/cam.html`) posts JPEG frames and the battery level. The viewer gets a server-sent event per frame and fetches `/snapshot.jpg`.
- **Phone screen and "direct" camera:** phoneOnDesk runs scrcpy's on-device server (`third_party/scrcpy`, Apache 2.0) over adb. It relays the H.264 video on `/phone/stream`, and the page decodes it with WebCodecs. Touches, scrolls and keys go back as scrcpy control messages on `/phone/control`.
- **Safety:**
  - Anything not on this computer needs the key from the QR code, so others on the same Wi‑Fi can’t watch or read the clipboard. The viewer port only listens on this computer.
  - It only answers its own host names, which blocks DNS rebinding.
  - It only answers cross-site requests from allowed origins. Another site may link to its pages, nothing more.
  - Phone and pairing routes answer only this computer.
- **Files:** settings, the certificate and the log live in `~/Library/Application Support/phoneOnDesk` on a Mac, or `~/.config/phoneOnDesk` on Linux.

## Build

```bash
go test ./...
./scripts/build-mac.sh      # dist/phoneOnDesk-mac.zip
./scripts/build-linux.sh    # needs appimagetool
```

Tag a version (`git tag v0.4.1 && git push --tags`) and GitHub Actions builds and publishes the downloads.

Options for development, as environment variables:
- `CAM_HTTP_PORT` (default 8080), `CAM_HTTPS_PORT` (8443) and `CAM_STREAM_PORT` (27183).
- `PHONEONDESK_ORIGINS`: a comma-separated allow list.
- `PHONEONDESK_DATA_DIR`.
