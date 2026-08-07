// Demo client for the liveness API.
//
// No build step and no dependencies: this is served straight out of the binary,
// and a demo that needs npm before anyone can look at it is a demo nobody looks
// at.
'use strict';

const els = {
  video: document.getElementById('preview'),
  canvas: document.getElementById('capture'),
  badge: document.getElementById('badge'),
  instruction: document.getElementById('instruction'),
  hint: document.getElementById('hint'),
  steps: document.getElementById('steps'),
  apikey: document.getElementById('apikey'),
  start: document.getElementById('start'),
  stop: document.getElementById('stop'),
  status: document.getElementById('status'),
  mode: document.getElementById('mode'),
};

// The API names challenges relative to the image. The preview is mirrored, the
// way a selfie camera always is, so the subject's left is the image's right —
// the instruction has to be flipped or everyone turns the wrong way.
const INSTRUCTIONS = {
  BLINK:      { text: 'Kedipkan mata',        hint: 'Pejamkan sebentar, lalu buka lagi.' },
  TURN_LEFT:  { text: 'Tengok ke KANAN Anda', hint: 'Pelan saja, sekitar seperempat putaran.' },
  TURN_RIGHT: { text: 'Tengok ke KIRI Anda',  hint: 'Pelan saja, sekitar seperempat putaran.' },
  NOD:        { text: 'Anggukkan kepala',     hint: 'Turunkan dagu, lalu angkat lagi.' },
  MOUTH_OPEN: { text: 'Buka mulut',           hint: 'Cukup lebar dan tahan sebentar.' },
};

// Roughly six frames a second. The measured budget is 149 ms for an ordinary
// frame, so asking for much more would only build a queue.
const FRAME_INTERVAL_MS = 160;

// Each challenge gets a moment before frames start counting. The turn and nod
// challenges take their baseline from the first frame they see, so a subject
// who has already started moving would be measured against their own turn.
const SETTLE_MS = 700;

const state = {
  session: null,
  seq: 0,
  stream: null,
  timer: null,
  inFlight: false,
  currentChallenge: null,
  settleUntil: 0,
  done: new Set(),
};

function setStatus(text, kind) {
  els.status.textContent = text;
  els.status.className = 'status' + (kind ? ' ' + kind : '');
}

function setBadge(text, kind) {
  if (!text) { els.badge.hidden = true; return; }
  els.badge.hidden = false;
  els.badge.textContent = text;
  els.badge.className = 'badge' + (kind ? ' ' + kind : '');
}

function renderSteps() {
  els.steps.replaceChildren();
  if (!state.session) return;

  for (const kind of state.session.challenges) {
    const li = document.createElement('li');
    li.textContent = (INSTRUCTIONS[kind] || { text: kind }).text;
    if (state.done.has(kind)) li.classList.add('done');
    else if (kind === state.currentChallenge) li.classList.add('active');
    els.steps.append(li);
  }
}

function showChallenge(kind) {
  if (kind && kind !== state.currentChallenge) {
    if (state.currentChallenge) state.done.add(state.currentChallenge);
    state.currentChallenge = kind;
    // Give the subject a moment to settle before their pose becomes the
    // baseline the movement is measured against.
    state.settleUntil = performance.now() + SETTLE_MS;
  }

  const copy = INSTRUCTIONS[kind] || { text: kind || '', hint: '' };
  els.instruction.textContent = copy.text;
  els.hint.textContent = copy.hint;
  renderSteps();
}

async function api(path, options = {}) {
  const res = await fetch(path, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      'X-API-Key': els.apikey.value.trim(),
      ...(options.headers || {}),
    },
  });

  let body = null;
  try { body = await res.json(); } catch { /* an empty or non-JSON body is fine */ }

  if (!res.ok) {
    const err = new Error(body?.error?.message || `HTTP ${res.status}`);
    err.status = res.status;
    err.code = body?.error?.code;
    throw err;
  }
  return body;
}

function captureFrame() {
  const video = els.video;
  if (!video.videoWidth) return null;

  // Downscale: the detector letterboxes to 320 anyway, and sending a 1080p
  // frame six times a second wastes bandwidth on pixels nothing reads.
  const target = 480;
  const scale = Math.min(1, target / Math.max(video.videoWidth, video.videoHeight));
  const w = Math.round(video.videoWidth * scale);
  const h = Math.round(video.videoHeight * scale);

  els.canvas.width = w;
  els.canvas.height = h;

  const ctx = els.canvas.getContext('2d');
  // Undo the CSS mirror: the preview is flipped for the subject's comfort, but
  // the server must see what the camera actually saw, or every left and right
  // in the pose estimate is inverted.
  ctx.save();
  ctx.setTransform(1, 0, 0, 1, 0, 0);
  ctx.drawImage(video, 0, 0, w, h);
  ctx.restore();

  return els.canvas.toDataURL('image/jpeg', 0.8);
}

async function sendFrame() {
  if (state.inFlight || !state.session) return;
  if (performance.now() < state.settleUntil) return;

  const frame = captureFrame();
  if (!frame) return;

  state.inFlight = true;
  state.seq += 1;

  try {
    const res = await api(`/v1/liveness/sessions/${state.session.session_id}/frames`, {
      method: 'POST',
      body: JSON.stringify({ seq: state.seq, nonce: state.session.nonce, frame }),
    });

    if (res.completed) {
      await finish();
      return;
    }
    if (res.challenge) showChallenge(res.challenge);

    if (res.advanced) setStatus('Bagus.', 'ok');
    else if (res.reason) setStatus(res.reason);
    else setStatus('');
  } catch (err) {
    // 422 is a verification failure; anything else is a problem with the
    // request or the service.
    if (err.status === 422 || err.status === 410) {
      stop();
      setBadge('GAGAL', 'bad');
      setStatus(err.message, 'bad');
      return;
    }
    if (err.status === 409) {
      // Another frame is being processed. Skipping this one is correct.
      return;
    }
    setStatus(err.message, 'warn');
  } finally {
    state.inFlight = false;
  }
}

async function finish() {
  stopCapture();
  try {
    const verdict = await api(`/v1/liveness/sessions/${state.session.session_id}/complete`, { method: 'POST' });
    if (verdict.passed) {
      setBadge('LOLOS', 'ok');
      setStatus('Verifikasi berhasil.', 'ok');
      state.done = new Set(state.session.challenges);
      state.currentChallenge = null;
      renderSteps();
    } else {
      setBadge('GAGAL', 'bad');
      setStatus(`Sesi berakhir dengan status ${verdict.state}.`, 'bad');
    }
  } catch (err) {
    setBadge('GAGAL', 'bad');
    setStatus(err.message, 'bad');
  }
  stop();
}

function stopCapture() {
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
}

function stop() {
  stopCapture();
  if (state.stream) {
    for (const track of state.stream.getTracks()) track.stop();
    state.stream = null;
  }
  els.video.srcObject = null;
  els.start.hidden = false;
  els.stop.hidden = true;
  els.apikey.disabled = false;
}

async function start() {
  if (!els.apikey.value.trim()) {
    setStatus('Masukkan API key dari .env terlebih dahulu.', 'warn');
    return;
  }

  setBadge(null);
  setStatus('Meminta akses kamera…');
  els.start.disabled = true;

  try {
    state.stream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: 'user', width: { ideal: 640 }, height: { ideal: 480 } },
      audio: false,
    });
  } catch (err) {
    els.start.disabled = false;
    if (err.name === 'NotAllowedError') {
      setStatus('Akses kamera ditolak. Izinkan lewat ikon di bilah alamat, lalu coba lagi.', 'bad');
    } else if (!window.isSecureContext) {
      setStatus('Kamera hanya bisa diakses lewat localhost atau HTTPS. Buka http://localhost:8080/demo.', 'bad');
    } else {
      setStatus(`Kamera tidak tersedia: ${err.message}`, 'bad');
    }
    return;
  }

  els.video.srcObject = state.stream;
  els.start.disabled = false;

  try {
    setStatus('Memulai sesi…');
    state.session = await api('/v1/liveness/sessions', { method: 'POST' });
  } catch (err) {
    stop();
    setStatus(err.status === 401 ? 'API key ditolak.' : err.message, 'bad');
    return;
  }

  state.seq = 0;
  state.done = new Set();
  state.currentChallenge = null;
  showChallenge(state.session.challenges[0]);

  els.start.hidden = true;
  els.stop.hidden = false;
  els.apikey.disabled = true;
  setStatus('Ikuti instruksi.');

  state.timer = setInterval(sendFrame, FRAME_INTERVAL_MS);
}

els.start.addEventListener('click', start);
els.stop.addEventListener('click', () => {
  stop();
  setStatus('Dibatalkan.');
  setBadge(null);
});

// A quick honesty check on load: tell the operator whether the models are
// actually in use, because a stub session proves only that the wiring works.
fetch('/healthz')
  .then((r) => r.json())
  .then((body) => { els.mode.textContent = body.pipeline || 'tidak diketahui'; })
  .catch(() => { els.mode.textContent = 'tidak dapat dihubungi'; });

if (!window.isSecureContext) {
  setStatus('Halaman ini bukan secure context; kamera tidak akan bisa diakses. Gunakan http://localhost:8080/demo.', 'warn');
}
