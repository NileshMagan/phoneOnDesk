// Plays a phone stream from /phone/stream onto a canvas with WebCodecs, and builds scrcpy
// control messages (touch, scroll, keys, text) for /phone/control.
// Stream framing from the server: [u8 type][u32 length][payload]. See phone-stream.mjs.

const MSG = { META: 0, CONFIG: 1, KEY: 2, DELTA: 3, END: 4 };
// Where phoneOnDesk is. Empty on its own pages; a page hosted elsewhere sets window.PHONE_BASE.
const PHONE_BASE = window.PHONE_BASE || '';

// "avc1.PPCCLL" from the SPS inside an Annex B config packet.
function avcCodec(config) {
  for (let i = 0; i + 4 < config.length; i++) {
    if (config[i] === 0 && config[i + 1] === 0 && config[i + 2] === 1 && (config[i + 3] & 0x1f) === 7) {
      const hex = (b) => b.toString(16).padStart(2, '0');
      return `avc1.${hex(config[i + 4])}${hex(config[i + 5])}${hex(config[i + 6])}`;
    }
  }
  return 'avc1.42e01f';
}

class PhoneVideo {
  // rotate: degrees clockwise (0/90/180/270), applied when drawing.
  constructor(source, canvas, { onFrameSize, onEnd } = {}) {
    this.source = source;
    this.canvas = canvas;
    this.ctx = canvas.getContext('2d');
    this.onFrameSize = onFrameSize;
    this.onEnd = onEnd;
    this.rotate = 0;
    this.frameW = 0;
    this.frameH = 0;
    this.lastFrame = null;
  }

  async start() {
    if (!('VideoDecoder' in window)) throw new Error('This browser cannot decode video (no WebCodecs). Try Chrome.');
    this.abort = new AbortController();
    const r = await fetch(`${PHONE_BASE}/phone/stream?source=${this.source}`, { signal: this.abort.signal });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `HTTP ${r.status}`);
    this.#read(r.body.getReader()).catch((e) => {
      if (!this.closed) this.onEnd?.(e.message);
    });
  }

  stop() {
    this.closed = true;
    this.abort?.abort();
    try { this.decoder?.close(); } catch {}
    this.lastFrame?.close();
  }

  async #read(reader) {
    let buf = new Uint8Array(0);
    for (;;) {
      const { value, done } = await reader.read();
      if (done) { if (!this.closed) this.onEnd?.('Stream ended'); return; }
      const merged = new Uint8Array(buf.length + value.length);
      merged.set(buf);
      merged.set(value, buf.length);
      buf = merged;
      let off = 0;
      while (buf.length - off >= 5) {
        const view = new DataView(buf.buffer, buf.byteOffset + off);
        const type = view.getUint8(0);
        const len = view.getUint32(1);
        if (buf.length - off - 5 < len) break;
        this.#message(type, buf.slice(off + 5, off + 5 + len));
        off += 5 + len;
      }
      buf = buf.slice(off);
    }
  }

  #message(type, data) {
    if (type === MSG.CONFIG) {
      this.config = data;
      this.gotKey = false;
      const codec = avcCodec(data);
      if (!this.decoder || this.decoder.state === 'closed') {
        this.decoder = new VideoDecoder({
          output: (frame) => this.#draw(frame),
          // A failed decoder is closed for good: end the stream so the page reconnects.
          error: () => { if (!this.closed) { this.closed = true; this.abort?.abort(); this.onEnd?.('The video decoder stopped'); } },
        });
      } else {
        this.decoder.reset();
      }
      this.decoder.configure({ codec, optimizeForLatency: true });
      this.ts = 0;
    } else if (type === MSG.KEY || type === MSG.DELTA) {
      if (!this.decoder || this.decoder.state !== 'configured') return;
      const key = type === MSG.KEY;
      if (!key && !this.gotKey) return;
      let payload = data;
      if (key) {
        // Annex B: send SPS/PPS along with every keyframe.
        payload = new Uint8Array(this.config.length + data.length);
        payload.set(this.config);
        payload.set(data, this.config.length);
        this.gotKey = true;
      }
      // Too far behind: drop deltas and wait for the next keyframe.
      if (!key && this.decoder.decodeQueueSize > 30) { this.gotKey = false; return; }
      this.decoder.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: (this.ts += 16_666), data: payload }));
    } else if (type === MSG.END) {
      this.closed = true;
      this.onEnd?.(new TextDecoder().decode(data));
    }
  }

  #draw(frame) {
    const w = frame.displayWidth, h = frame.displayHeight;
    if (w !== this.frameW || h !== this.frameH) {
      this.frameW = w;
      this.frameH = h;
      this.#resizeCanvas();
    }
    this.lastFrame?.close();
    this.lastFrame = frame;
    this.lastAt = Date.now();
    this.redraw();
  }

  #resizeCanvas() {
    const sideways = this.rotate % 180 !== 0;
    this.canvas.width = sideways ? this.frameH : this.frameW;
    this.canvas.height = sideways ? this.frameW : this.frameH;
    this.onFrameSize?.(this.canvas.width, this.canvas.height);
  }

  setRotate(deg) {
    this.rotate = ((deg % 360) + 360) % 360;
    if (this.frameW) { this.#resizeCanvas(); this.redraw(); }
  }

  redraw() {
    const f = this.lastFrame;
    if (!f) return;
    const { ctx, canvas } = this;
    ctx.save();
    ctx.translate(canvas.width / 2, canvas.height / 2);
    ctx.rotate((this.rotate * Math.PI) / 180);
    ctx.drawImage(f, -this.frameW / 2, -this.frameH / 2, this.frameW, this.frameH);
    ctx.restore();
  }
}

// ---- scrcpy control messages ----
const GENERIC_FINGER = 0xfffffffffffffffen; // pointer id -2
const KEY = { HOME: 3, BACK: 4, ENTER: 66, DEL: 67, TAB: 61, ESCAPE: 111, FORWARD_DEL: 112,
  DPAD_UP: 19, DPAD_DOWN: 20, DPAD_LEFT: 21, DPAD_RIGHT: 22, MOVE_HOME: 122, MOVE_END: 123,
  APP_SWITCH: 187, POWER: 26, VOLUME_UP: 24, VOLUME_DOWN: 25 };

const control = {
  touch(action, x, y, w, h) { // action: 0 down, 1 up, 2 move
    const v = new DataView(new ArrayBuffer(32));
    v.setUint8(0, 2);
    v.setUint8(1, action);
    v.setBigUint64(2, GENERIC_FINGER);
    v.setInt32(10, Math.round(x)); v.setInt32(14, Math.round(y));
    v.setUint16(18, w); v.setUint16(20, h);
    v.setUint16(22, action === 1 ? 0 : 0xffff); // pressure
    v.setUint32(24, 0); v.setUint32(28, 0);
    return v.buffer;
  },
  scroll(x, y, w, h, hscroll, vscroll) { // scroll amounts in [-16, 16]
    const fp = (f) => Math.max(-0x8000, Math.min(0x7fff, Math.round((Math.max(-16, Math.min(16, f)) / 16) * 0x8000)));
    const v = new DataView(new ArrayBuffer(21));
    v.setUint8(0, 3);
    v.setInt32(1, Math.round(x)); v.setInt32(5, Math.round(y));
    v.setUint16(9, w); v.setUint16(11, h);
    v.setInt16(13, fp(hscroll)); v.setInt16(15, fp(vscroll));
    v.setUint32(17, 0);
    return v.buffer;
  },
  key(action, keycode, metastate = 0) { // action: 0 down, 1 up
    const v = new DataView(new ArrayBuffer(14));
    v.setUint8(0, 0);
    v.setUint8(1, action);
    v.setUint32(2, keycode); v.setUint32(6, 0); v.setUint32(10, metastate);
    return v.buffer;
  },
  text(str) {
    const bytes = new TextEncoder().encode(str.slice(0, 300));
    const v = new DataView(new ArrayBuffer(5 + bytes.length));
    v.setUint8(0, 1);
    v.setUint32(1, bytes.length);
    new Uint8Array(v.buffer, 5).set(bytes);
    return v.buffer;
  },
};

// Batches control messages so a drag sends one request per animation frame, not per event, and
// sends one batch at a time: parallel requests can land out of order and leave a finger down.
const controlQueue = [];
let controlBusy = false;
function sendControl(...msgs) {
  controlQueue.push(...msgs);
  if (!controlBusy) { controlBusy = true; requestAnimationFrame(flushControl); }
}
async function flushControl() {
  const parts = controlQueue.splice(0);
  try { await fetch(`${PHONE_BASE}/phone/control`, { method: 'POST', body: new Blob(parts) }); } catch {}
  if (controlQueue.length) requestAnimationFrame(flushControl);
  else controlBusy = false;
}
function pressKey(keycode) {
  sendControl(control.key(0, keycode), control.key(1, keycode));
}
