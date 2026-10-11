// Printer cam: phone POSTs JPEG frames, Mac watches an MJPEG stream. No dependencies.
// `node server.mjs --app` quits on its own once the viewer tab has been closed.
import https from 'node:https';
import http from 'node:http';
import os from 'node:os';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const dir = dirname(fileURLToPath(import.meta.url));
const PORT = Number(process.env.CAM_HTTPS_PORT || 8443);
const HTTP_PORT = Number(process.env.CAM_HTTP_PORT || 8080);
const APP_MODE = process.argv.includes('--app');
const QUIT_AFTER_MS = 20_000;
const page = (name) => readFileSync(join(dir, name));

// Addresses the phone can reach this Mac on.
function lanAddresses() {
  const out = { lan: [], tailscale: [] };
  for (const list of Object.values(os.networkInterfaces())) {
    for (const a of list || []) {
      if (a.family !== 'IPv4' || a.internal) continue;
      (a.address.startsWith('100.') ? out.tailscale : out.lan).push(a.address);
    }
  }
  // Home Wi-Fi (192.168.x) first, so the QR code points at it rather than a VPN or hotspot.
  out.lan.sort((a, b) => b.startsWith('192.168.') - a.startsWith('192.168.'));
  return out;
}
const localName = (() => {
  try { return execFileSync('scutil', ['--get', 'LocalHostName']).toString().trim().toLowerCase() + '.local'; }
  catch { return null; }
})();

// Self-signed cert covering every current address. Only regenerated when a new address appears,
// so the phone's one-time "proceed anyway" keeps working.
function ensureCert() {
  const certDir = join(dir, 'certs');
  const ipsFile = join(certDir, 'ips.txt');
  const addrs = lanAddresses();
  const wanted = [...addrs.lan, ...addrs.tailscale];
  const have = existsSync(ipsFile) ? readFileSync(ipsFile, 'utf8').split('\n').filter(Boolean) : [];
  const covered = existsSync(join(certDir, 'cert.pem')) && wanted.every((ip) => have.includes(ip));
  if (!covered) {
    mkdirSync(certDir, { recursive: true });
    const ips = [...new Set([...have, ...wanted, '127.0.0.1'])];
    const san = [...ips.map((ip) => `IP:${ip}`), 'DNS:localhost', ...(localName ? [`DNS:${localName}`] : [])].join(',');
    execFileSync('openssl', [
      'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '3650',
      '-keyout', join(certDir, 'key.pem'), '-out', join(certDir, 'cert.pem'),
      '-subj', '/CN=printer-cam', '-addext', `subjectAltName=${san}`,
    ], { stdio: 'ignore' });
    writeFileSync(ipsFile, ips.join('\n') + '\n');
  }
  return { key: readFileSync(join(certDir, 'key.pem')), cert: readFileSync(join(certDir, 'cert.pem')) };
}

let frame = null;
let status = { level: null, charging: null, at: 0, w: 0, h: 0 };
const watchers = new Set();
let quitTimer = null;

function scheduleQuit() {
  if (!APP_MODE || watchers.size) return;
  clearTimeout(quitTimer);
  quitTimer = setTimeout(() => {
    if (!watchers.size) shutdown('viewer closed');
  }, QUIT_AFTER_MS);
}

function shutdown(reason) {
  console.log(`stopping: ${reason}`);
  process.exit(0);
}

function sendFrame(res) {
  res.write(`--frame\r\nContent-Type: image/jpeg\r\nContent-Length: ${frame.length}\r\n\r\n`);
  res.write(frame);
  res.write('\r\n');
}

const isLoopback = (req) => ['127.0.0.1', '::1', '::ffff:127.0.0.1'].includes(req.socket.remoteAddress);

function handle(req, res) {
  const url = new URL(req.url, 'https://x');

  if (req.method === 'POST' && url.pathname === '/frame') {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      frame = Buffer.concat(chunks);
      const level = url.searchParams.get('battery');
      status = {
        level: level === null || level === '' ? null : Number(level),
        charging: url.searchParams.get('charging') === '1',
        at: Date.now(),
        w: Number(url.searchParams.get('w')) || 0,
        h: Number(url.searchParams.get('h')) || 0,
      };
      for (const w of watchers) sendFrame(w);
      res.writeHead(204).end();
    });
    return;
  }

  if (url.pathname === '/stream') {
    res.writeHead(200, {
      'Content-Type': 'multipart/x-mixed-replace; boundary=frame',
      'Cache-Control': 'no-store',
      Connection: 'close',
    });
    watchers.add(res);
    clearTimeout(quitTimer);
    if (frame) sendFrame(res);
    req.on('close', () => {
      watchers.delete(res);
      scheduleQuit();
    });
    return;
  }

  if (url.pathname === '/snapshot.jpg') {
    if (!frame) return res.writeHead(404).end('no frame yet');
    return res.writeHead(200, { 'Content-Type': 'image/jpeg', 'Cache-Control': 'no-store' }).end(frame);
  }

  if (url.pathname === '/status') {
    res.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
    return res.end(JSON.stringify({ ...status, ageMs: status.at ? Date.now() - status.at : null }));
  }

  if (url.pathname === '/info') {
    const addrs = lanAddresses();
    const phoneUrls = {
      named: localName ? `https://${localName}:${PORT}/cam` : null,
      lan: addrs.lan.map((ip) => `https://${ip}:${PORT}/cam`),
      tailscale: addrs.tailscale.map((ip) => `https://${ip}:${PORT}/cam`),
    };
    res.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
    return res.end(JSON.stringify({ phoneUrls, appMode: APP_MODE }));
  }

  if (req.method === 'POST' && url.pathname === '/quit') {
    if (!isLoopback(req)) return res.writeHead(403).end();
    res.writeHead(204).end();
    return setTimeout(() => shutdown('stop button'), 100);
  }

  const html = { '/': 'view.html', '/view': 'view.html', '/cam': 'cam.html' }[url.pathname];
  if (html) {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' });
    return res.end(page(html));
  }
  res.writeHead(404).end();
}

// Phone needs HTTPS for camera access; the Mac viewer can use plain HTTP and skip the cert warning.
https.createServer(ensureCert(), handle)
  .listen(PORT, '0.0.0.0', () => console.log(`phone:  https://<mac-ip>:${PORT}/cam`));
http.createServer(handle)
  .listen(HTTP_PORT, '0.0.0.0', () => console.log(`viewer: http://localhost:${HTTP_PORT}/`));

// In app mode, give the browser a minute to open the viewer before deciding nobody's watching.
if (APP_MODE) quitTimer = setTimeout(() => { if (!watchers.size) shutdown('viewer never opened'); }, 60_000);
