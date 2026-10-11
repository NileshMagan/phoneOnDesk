# phoneOnDesk

Use an Android phone on your desk from your Mac:

- **Camera:** watch the phone's camera, e.g. a 3D printer, along with its battery level.
- **Clipboard:** send text both ways.
- **Phone screen:** see and control it in a window, via [scrcpy](https://github.com/Genymobile/scrcpy).

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
- Phone screen: the `/adb/*` and `/screen` endpoints only answer from the Mac itself. They pair or connect over adb wireless debugging and launch scrcpy.
- The phone needs HTTPS for camera access, so the server makes a self-signed cert in `certs/` covering the Mac's current IPs and `.local` name. It only regenerates when a new IP appears.
- Logs: `~/Library/Logs/phoneondesk.log`.
