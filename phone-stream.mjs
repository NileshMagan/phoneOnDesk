// Streams the phone's screen (or camera) into the browser using scrcpy's own on-device server.
// Mac side: adb pushes scrcpy-server, forwards a local TCP port to it, and we relay H.264 packets
// to viewers over a long-lived HTTP response. The page decodes them with WebCodecs.
//
// Protocol notes (scrcpy v3, forward tunnel): the first socket gets a dummy 0 byte, then
// codec meta (u32 codec id, u32 width, u32 height), then packets: u64 pts+flags, u32 length, data.
// The second socket is the control channel.
import net from 'node:net';
import { execFile, execFileSync, spawn } from 'node:child_process';
import { realpathSync } from 'node:fs';
import { dirname, join } from 'node:path';

const FLAG_CONFIG = 1n << 63n;
const FLAG_KEY = 1n << 62n;
const IDLE_STOP_MS = 15_000;

// Message types sent to the browser: [u8 type][u32 length][payload]
export const MSG = { META: 0, CONFIG: 1, KEY: 2, DELTA: 3, END: 4 };

function frame(type, payload) {
  const head = Buffer.alloc(5);
  head.writeUInt8(type, 0);
  head.writeUInt32BE(payload.length, 1);
  return Buffer.concat([head, payload]);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export class PhoneStream {
  constructor({ serial, source, port, env }) {
    this.serial = serial;
    this.source = source; // 'display' | 'camera'
    this.port = port;
    this.env = env;
    this.clients = new Set();
    this.meta = null; // { codec, width, height }
    this.config = null; // latest SPS/PPS packet
    this.gop = []; // packets since the last keyframe, for late joiners
    this.ready = null;
    this.stopped = false;
    this.idleTimer = null;
    this.onEnd = null;
  }

  adb(args) {
    return new Promise((resolve, reject) => {
      execFile('adb', ['-s', this.serial, ...args], { env: this.env, timeout: 20_000 }, (err, stdout, stderr) =>
        err ? reject(new Error(`${stderr || err.message}`.trim())) : resolve(stdout));
    });
  }

  start() {
    this.ready ??= this.#start().catch((e) => {
      this.stop(e.message);
      throw e;
    });
    return this.ready;
  }

  async #start() {
    const scrcpyBin = realpathSync(execFileSync('which', ['scrcpy'], { env: this.env }).toString().trim());
    const version = execFileSync(scrcpyBin, ['--version'], { env: this.env }).toString().match(/scrcpy (\S+)/)[1];
    const jar = join(dirname(scrcpyBin), '..', 'share', 'scrcpy', 'scrcpy-server');
    const scid = Math.floor(Math.random() * 0x7fffffff).toString(16).padStart(8, '0');

    await this.adb(['push', jar, '/data/local/tmp/scrcpy-server.jar']);
    await this.adb(['forward', `tcp:${this.port}`, `localabstract:scrcpy_${scid}`]);

    const params = [
      `scid=${scid}`, 'log_level=info', 'tunnel_forward=true', 'audio=false',
      'video_codec=h264', 'send_device_meta=false', 'cleanup=true',
    ];
    if (this.source === 'camera') {
      params.push('video_source=camera', 'camera_facing=back', 'max_size=1280', 'control=false');
    } else {
      params.push('max_size=1280', 'stay_awake=true');
    }
    this.proc = spawn('adb', ['-s', this.serial, 'shell', 'CLASSPATH=/data/local/tmp/scrcpy-server.jar',
      'app_process', '/', 'com.genymobile.scrcpy.Server', version, ...params], { env: this.env });
    this.log = '';
    this.proc.stdout.on('data', (d) => (this.log += d));
    this.proc.stderr.on('data', (d) => (this.log += d));
    this.proc.on('exit', () => this.stop(this.log.match(/ERROR:.*$/m)?.[0] || 'phone stream ended'));

    this.video = await this.#connect(true);
    if (this.source === 'display') {
      this.control = await this.#connect(false);
      this.control.on('data', () => {}); // device clipboard/acks: not used
      this.control.on('error', () => {});
    }
    this.#readVideo();
  }

  // adb accepts the TCP connection before the phone server is listening, so retry until
  // the dummy byte arrives (first socket) or the connection stays open (second).
  async #connect(expectDummy) {
    for (let i = 0; i < 100 && !this.stopped; i++) {
      const sock = await new Promise((resolve) => {
        const s = net.connect(this.port, '127.0.0.1');
        s.once('connect', () => resolve(s));
        s.once('error', () => resolve(null));
      });
      if (sock) {
        if (!expectDummy) return sock;
        const ok = await new Promise((resolve) => {
          sock.once('data', (d) => {
            sock.pause(); // hold anything after the dummy byte until the reader attaches
            if (d.length > 1) sock.unshift(d.subarray(1));
            resolve(true);
          });
          sock.once('close', () => resolve(false));
        });
        if (ok) return sock;
        sock.destroy();
      }
      await sleep(100);
    }
    throw new Error(this.log.match(/ERROR:.*$/m)?.[0] || 'Could not connect to the phone');
  }

  #readVideo() {
    let buf = Buffer.alloc(0);
    let header = null;
    this.video.on('data', (chunk) => {
      buf = buf.length ? Buffer.concat([buf, chunk]) : chunk;
      for (;;) {
        if (!this.meta) {
          if (buf.length < 12) return;
          const id = buf.readUInt32BE(0);
          this.meta = { codec: id, width: buf.readUInt32BE(4), height: buf.readUInt32BE(8) };
          buf = buf.subarray(12);
          this.#broadcast(MSG.META, Buffer.from(JSON.stringify(this.meta)));
          continue;
        }
        if (!header) {
          if (buf.length < 12) return;
          header = { flags: buf.readBigUInt64BE(0), len: buf.readUInt32BE(8) };
          buf = buf.subarray(12);
        }
        if (buf.length < header.len) return;
        const data = Buffer.from(buf.subarray(0, header.len));
        buf = buf.subarray(header.len);
        this.#packet(header.flags, data);
        header = null;
      }
    });
    this.video.resume();
    this.video.on('close', () => this.stop('phone stream closed'));
    this.video.on('error', () => {});
  }

  #packet(flags, data) {
    if (flags & FLAG_CONFIG) {
      this.config = data;
      this.gop = [];
      this.#broadcast(MSG.CONFIG, data);
      return;
    }
    const key = (flags & FLAG_KEY) !== 0n;
    if (key) this.gop = [];
    this.gop.push({ key, data });
    for (const c of this.clients) {
      // A slow viewer skips ahead to the next keyframe instead of falling behind.
      if (c.res.writableLength > 4_000_000) c.needKey = true;
      if (c.needKey && !key) continue;
      if (c.needKey && this.config) c.res.write(frame(MSG.CONFIG, this.config));
      c.needKey = false;
      c.res.write(frame(key ? MSG.KEY : MSG.DELTA, data));
    }
  }

  #broadcast(type, payload) {
    for (const c of this.clients) c.res.write(frame(type, payload));
  }

  addClient(res) {
    clearTimeout(this.idleTimer);
    const client = { res, needKey: false };
    if (this.meta) res.write(frame(MSG.META, Buffer.from(JSON.stringify(this.meta))));
    if (this.config) res.write(frame(MSG.CONFIG, this.config));
    for (const p of this.gop) res.write(frame(p.key ? MSG.KEY : MSG.DELTA, p.data));
    this.clients.add(client);
    return () => {
      this.clients.delete(client);
      if (!this.clients.size) this.idleTimer = setTimeout(() => this.stop('no viewers'), IDLE_STOP_MS);
    };
  }

  sendControl(buf) {
    if (this.control && !this.control.destroyed) this.control.write(buf);
  }

  stop(reason = 'stopped') {
    if (this.stopped) return;
    this.stopped = true;
    clearTimeout(this.idleTimer);
    for (const c of this.clients) c.res.end(frame(MSG.END, Buffer.from(reason)));
    this.clients.clear();
    this.video?.destroy();
    this.control?.destroy();
    this.proc?.kill();
    execFile('adb', ['-s', this.serial, 'forward', '--remove', `tcp:${this.port}`], { env: this.env }, () => {});
    this.onEnd?.(reason);
  }
}
