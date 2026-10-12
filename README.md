# phoneOnDesk

Use an Android phone on your desk from your Mac:

- **Camera:** watch the phone's camera, e.g. a 3D printer, along with its battery level.
- **Clipboard:** send text both ways.
- **Phone screen:** see and control it right in the page, next to the camera: click to tap, drag to swipe, scroll, type. Or pop it out into its own window.
- **Direct camera:** once the phone is paired, stream the camera straight over the debugging link, so it keeps running while you use other apps on the phone.

No npm dependencies, just Node. The phone screen needs `brew install scrcpy`, which includes adb.

## Use

1. `./build-app.sh` once. It puts **phoneOnDesk.app** in `~/Applications`.
2. Open the app. Your browser opens the viewer with a QR code and setup steps for the phone.
3. Close the tab when you're done. The server stops itself about 20 seconds later (or press **Stop**).

Without the app: `node server.mjs`. Viewer is at http://localhost:8080 and the phone page is at `https://<mac-ip>:8443/cam`.

## How it works

- The phone page (`cam.html`) grabs camera frames and POSTs JPEGs plus battery level to the server.
- The viewer (`view.html`) gets a server-sent event per frame and fetches `/snapshot.jpg`.
- Clipboard: `/clip` holds one slot each way. On the Mac page, ⌘V sends text to the phone.
- Phone screen and direct camera: `phone-stream.mjs` runs scrcpy's own on-device server over adb (wireless debugging) and relays its H.264 packets on `/phone/stream`. `phone-video.js` decodes them in the page with WebCodecs, and sends touch, scroll and key events back as scrcpy control messages on `/phone/control`. These endpoints, plus `/adb/*` and `/screen` (pop-out window), only answer from the Mac itself.
- The phone needs HTTPS for camera access, so the server makes a self-signed cert in `certs/` covering the Mac's current IPs and `.local` name. It only regenerates when a new IP appears.
- Logs: `~/Library/Logs/phoneondesk.log`.
