# Printer Cam

Watch your 3D printer from your Mac, using an Android phone as the camera. Shows the phone's battery too.

No dependencies, just Node.

## Use

1. `./build-app.sh` once. It puts **Printer Cam.app** in `~/Applications`.
2. Open the app. Your browser opens the viewer with a QR code and setup steps for the phone.
3. Close the tab when you're done. The server stops itself about 20 seconds later (or press **Stop**).

Without the app: `node server.mjs`. Viewer is at http://localhost:8080 and the phone page is at `https://<mac-ip>:8443/cam`.

## How it works

- The phone page (`cam.html`) grabs camera frames and POSTs JPEGs plus battery level to the server.
- The viewer (`view.html`) shows them as an MJPEG stream, and polls `/status` for battery and freshness.
- The phone needs HTTPS for camera access, so the server makes a self-signed cert in `certs/` covering the Mac's current IPs and `.local` name. It only regenerates when a new IP appears.
- Logs: `~/Library/Logs/printer-cam.log`.
