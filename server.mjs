// phoneOnDesk: phone POSTs JPEG frames; the Mac viewer gets a ping per frame and fetches it. No dependencies.
// `node server.mjs --app` quits on its own once the viewer tab has been closed.
import https from 'node:https';
import http from 'node:http';
import os from 'node:os';
import { execFile, execFileSync, spawn } from 'node:child_process';
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
      '-subj', '/CN=phoneondesk', '-addext', `subjectAltName=${san}`,
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
  closeScreen();
  process.exit(0);
}

function announceFrame() {
  for (const w of watchers) w.write(`data: ${status.at}\n\n`);
}

const isLoopback = (req) => ['127.0.0.1', '::1', '::ffff:127.0.0.1'].includes(req.socket.remoteAddress);

function readBody(req, limit = 1_000_000) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (c) => {
      size += c.length;
      if (size > limit) { reject(new Error('too large')); req.destroy(); }
      else chunks.push(c);
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
  });
}

const json = (res, code, body) => {
  res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(body));
};

// Clipboard hand-off: one slot each way.
const clip = { toMac: { text: '', at: 0 }, toPhone: { text: '', at: 0 } };

// Phone screen via adb + scrcpy. Apps launched from Finder don't get Homebrew on PATH.
const toolEnv = { ...process.env, PATH: `${process.env.PATH || ''}:/opt/homebrew/bin:/usr/local/bin` };
const hasTool = (name) => {
  try { execFileSync('which', [name], { env: toolEnv, stdio: 'ignore' }); return true; } catch { return false; }
};
const run = (cmd, args, timeout = 15_000) =>
  new Promise((resolve) => {
    execFile(cmd, args, { env: toolEnv, timeout }, (err, stdout, stderr) =>
      resolve({ ok: !err, out: `${stdout}${stderr}`.trim() }));
  });

async function adbDevices() {
  const { out } = await run('adb', ['devices', '-l']);
  return out.split('\n').slice(1).filter((l) => l.trim() && !l.startsWith('*')).map((l) => {
    const [serial, state] = l.trim().split(/\s+/);
    const model = (l.match(/model:(\S+)/) || [])[1]?.replace(/_/g, ' ') || serial;
    return { serial, state, model };
  });
}

let scrcpyProc = null;
const SCREEN_APP = process.env.SCREEN_APP && existsSync(process.env.SCREEN_APP) ? process.env.SCREEN_APP : null;
const closeScreen = () => {
  scrcpyProc?.kill();
  // The helper execs scrcpy, so match it by its window title.
  try { execFileSync('pkill', ['-f', 'scrcpy .*--window-title=Phone screen']); } catch {}
};

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
      announceFrame();
      res.writeHead(204).end();
    });
    return;
  }

  // Server-sent events: one message per new frame. Open connections count as viewers.
  if (url.pathname === '/events') {
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', Connection: 'keep-alive' });
    res.write('retry: 2000\n\n');
    if (frame) res.write(`data: ${status.at}\n\n`);
    watchers.add(res);
    clearTimeout(quitTimer);
    const keepAlive = setInterval(() => res.write(': ping\n\n'), 15_000);
    req.on('close', () => {
      clearInterval(keepAlive);
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

  if (url.pathname === '/clip') {
    if (req.method === 'GET') return json(res, 200, clip);
    if (req.method === 'POST') {
      const slot = url.searchParams.get('to') === 'phone' ? 'toPhone' : 'toMac';
      readBody(req).then(
        (text) => { clip[slot] = { text, at: Date.now() }; json(res, 200, { ok: true }); },
        () => json(res, 413, { error: 'Too much text' }),
      );
      return;
    }
  }

  // Phone-control endpoints only answer the Mac itself, never the network.
  if (url.pathname.startsWith('/adb') || url.pathname === '/screen') {
    if (!isLoopback(req)) return json(res, 403, { error: 'Mac only' });

    if (url.pathname === '/adb' && req.method === 'GET') {
      if (!hasTool('adb') || !hasTool('scrcpy')) return json(res, 200, { tools: false, devices: [] });
      adbDevices().then((devices) => json(res, 200, { tools: true, devices, screenOpen: !!scrcpyProc }));
      return;
    }

    if (req.method === 'POST' && (url.pathname === '/adb/pair' || url.pathname === '/adb/connect')) {
      readBody(req).then(async (body) => {
        const { target = '', code = '' } = JSON.parse(body || '{}');
        if (!/^[\w.-]+:\d+$/.test(target)) return json(res, 400, { ok: false, out: 'Use the IP address:port shown on the phone, e.g. 192.168.1.20:37123' });
        const args = url.pathname === '/adb/pair' ? ['pair', target, String(code).replace(/\D/g, '')] : ['connect', target];
        const result = await run('adb', args);
        const ok = result.ok && !/fail|unable|error/i.test(result.out);
        const out = result.out || (ok ? 'Done' : "Couldn't reach the phone. Check the address and that Wireless debugging is on.");
        json(res, 200, { ok, out });
      });
      return;
    }

    if (url.pathname === '/screen' && req.method === 'POST') {
      if (scrcpyProc) {
        if (SCREEN_APP) spawn('open', ['-a', SCREEN_APP]);
        return json(res, 200, { ok: true, out: 'Phone screen brought to the front' });
      }
      adbDevices().then((devices) => {
        const device = devices.find((d) => d.state === 'device');
        if (!device) return json(res, 409, { ok: false, out: 'No phone connected. Pair it first.' });
        const args = ['-s', device.serial, '--window-title=Phone screen', '--no-audio', '--stay-awake'];
        // `open -W` lasts as long as the window does, so scrcpyProc still tracks whether it's open.
        scrcpyProc = SCREEN_APP
          ? spawn('open', ['-W', '-n', '-a', SCREEN_APP, '--args', ...args], { env: toolEnv, stdio: ['ignore', 'ignore', 'pipe'] })
          : spawn('scrcpy', args, { env: toolEnv, stdio: ['ignore', 'ignore', 'pipe'] });
        let err = '';
        scrcpyProc.stderr.on('data', (d) => (err += d));
        scrcpyProc.on('exit', (code) => {
          if (code) console.log(`phone screen exited ${code}: ${err.slice(-500)}`);
          scrcpyProc = null;
        });
        json(res, 200, { ok: true, out: `Opening ${device.model}` });
      });
      return;
    }
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
