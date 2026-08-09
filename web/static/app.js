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
  timer: document.getElementById('timer'),
  timerArc: document.getElementById('timerArc'),
  timerText: document.getElementById('timerText'),
  instruction: document.getElementById('instruction'),
  hint: document.getElementById('hint'),
  steps: document.getElementById('steps'),
  pass: document.getElementById('pass'),
  passMark: document.getElementById('passMark'),
  passTitle: document.getElementById('passTitle'),
  passNext: document.getElementById('passNext'),
  passAction: document.getElementById('passAction'),
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
//
// Short, because the budget is short: at a five-second challenge this is
// already a twelfth of it, and people take longer than this to read the
// instruction anyway.
const SETTLE_MS = 400;

// How long the "step passed" confirmation stays up.
//
// Kept short because it is not free. The server starts the next challenge's
// clock the moment the satisfying frame is accepted, so every millisecond spent
// celebrating comes out of the next step's budget. Long enough to register,
// short enough that the countdown underneath it is still worth having.
const PASS_MS = 1000;

// The countdown redraws far more often than frames arrive, so the number moves
// smoothly instead of stepping six times a second.
const TICK_MS = 100;

// Circumference of the progress ring: 2 * pi * 19, matching the SVG.
const ARC_LENGTH = 119.38;

const state = {
  session: null,
  seq: 0,
  stream: null,
  timer: null,
  ticker: null,
  inFlight: false,
  currentChallenge: null,
  settleUntil: 0,
  passTimer: null,
  lastReason: '',
  done: new Set(),

  // The countdown is interpolated between server responses. Storing when the
  // last figure arrived is what lets it run down smoothly without the client
  // ever deciding for itself how much time is left.
  secondsAtSync: 0,
  syncedAt: 0,
  challengeSeconds: 0,
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

function formatSeconds(s) {
  return String(Math.max(0, Math.ceil(s)));
}

function renderSteps() {
  els.steps.replaceChildren();
  if (!state.session) return;

  const allowance = state.challengeSeconds ? `${Math.round(state.challengeSeconds)} dtk` : '';

  for (const kind of state.session.challenges) {
    const li = document.createElement('li');

    const label = document.createElement('span');
    label.textContent = (INSTRUCTIONS[kind] || { text: kind }).text;
    li.append(label);

    if (allowance) {
      const time = document.createElement('span');
      time.className = 'allowance';
      time.textContent = allowance;
      li.append(time);
    }

    if (state.done.has(kind)) li.classList.add('done');
    else if (kind === state.currentChallenge) li.classList.add('active');

    els.steps.append(li);
  }
}

// remainingSeconds interpolates from the last figure the server sent.
function remainingSeconds() {
  if (!state.syncedAt) return 0;
  const elapsed = (performance.now() - state.syncedAt) / 1000;
  return Math.max(0, state.secondsAtSync - elapsed);
}

function syncCountdown(seconds) {
  if (typeof seconds !== 'number' || Number.isNaN(seconds)) return;
  state.secondsAtSync = seconds;
  state.syncedAt = performance.now();
}

function drawCountdown() {
  if (!state.session || !state.challengeSeconds) {
    els.timer.hidden = true;
    return;
  }

  const left = remainingSeconds();
  els.timer.hidden = false;
  els.timerText.textContent = formatSeconds(left);

  const fraction = Math.min(1, Math.max(0, left / state.challengeSeconds));
  els.timerArc.style.strokeDashoffset = String(ARC_LENGTH * (1 - fraction));

  els.timer.classList.toggle('warn', left <= 5 && left > 3);
  els.timer.classList.toggle('critical', left <= 3);
}

function showChallenge(kind, seconds) {
  if (kind && kind !== state.currentChallenge) {
    if (state.currentChallenge) state.done.add(state.currentChallenge);
    state.currentChallenge = kind;
    // Give the subject a moment to settle before their pose becomes the
    // baseline the movement is measured against.
    state.settleUntil = performance.now() + SETTLE_MS;
  }

  syncCountdown(seconds);

  const copy = INSTRUCTIONS[kind] || { text: kind || '', hint: '' };
  els.instruction.textContent = copy.text;
  els.hint.textContent = copy.hint;
  renderSteps();
  drawCountdown();
}

// showPass confirms a completed step and previews the next one.
//
// Frames are paused while it is up, by pushing the settle deadline out past it.
// That is not only cosmetic: the turn and nod challenges take their baseline
// from the first frame they see, so a frame captured while the subject is still
// reacting to the previous step would become the pose everything is measured
// against.
function showPass(finished, next) {
  const done = INSTRUCTIONS[finished];
  const upcoming = INSTRUCTIONS[next];

  els.pass.classList.remove('bad');
  els.passMark.textContent = '✓';
  els.passTitle.textContent = done ? `${done.text} — berhasil` : 'Langkah berhasil';
  els.passNext.textContent = upcoming ? `Berikutnya: ${upcoming.text}` : 'Menyelesaikan verifikasi…';
  els.passAction.hidden = true;
  els.pass.hidden = false;

  // Frames resume after the popup, then after the usual settle.
  state.settleUntil = performance.now() + PASS_MS + SETTLE_MS;

  if (state.passTimer) clearTimeout(state.passTimer);
  state.passTimer = setTimeout(() => {
    els.pass.hidden = true;
    state.passTimer = null;
  }, PASS_MS);
}

// showRetry says the step ran out of time and is starting again.
//
// It behaves like the between-steps confirmation rather than like a verdict:
// the session is still alive, the new attempt's clock is already running, and
// frames stay paused so the fresh attempt does not take its baseline from a
// subject still finishing the movement that ran out of time.
function showRetry(kind, left) {
  const step = INSTRUCTIONS[kind];
  const chances = left > 0 ? `Sisa ${left} kesempatan.` : 'Kesempatan terakhir.';

  els.pass.classList.add('bad');
  els.passMark.textContent = '↻';
  els.passTitle.textContent = 'Waktu habis — ulangi langkah ini';

  // The last per-frame reason, when there was one, is what the subject
  // actually needs. Without it this overlay says the step ran out of time and
  // nothing about why — which is how a session where every frame was refused
  // for being too far from the camera looked like a movement that simply was
  // not big enough.
  els.passNext.textContent = state.lastReason
    ? `${state.lastReason}. ${chances}`
    : `${step ? step.text : 'Langkah ini'}. ${chances}`;
  els.passAction.hidden = true;
  els.pass.hidden = false;

  state.settleUntil = performance.now() + PASS_MS + SETTLE_MS;

  if (state.passTimer) clearTimeout(state.passTimer);
  state.passTimer = setTimeout(() => {
    els.pass.hidden = true;
    els.pass.classList.remove('bad');
    state.passTimer = null;
  }, PASS_MS);
}

// showResult ends the session on screen and waits to be acknowledged.
//
// Unlike the between-steps confirmation it does not dismiss itself: the session
// is over either way, so there is no clock left to spend, and a verdict that
// vanishes before it is read is how this ended up as a line of small red text
// in the side panel in the first place.
function showResult(ok, title, detail, actionLabel) {
  if (state.passTimer) { clearTimeout(state.passTimer); state.passTimer = null; }

  els.pass.classList.toggle('bad', !ok);
  els.passMark.textContent = ok ? '✓' : '✕';
  els.passTitle.textContent = title;
  els.passNext.textContent = detail;
  els.passAction.textContent = actionLabel;
  els.passAction.hidden = false;
  els.pass.hidden = false;
}

function showFail(title, detail) {
  showResult(false, title, detail, 'Ulangi dari awal');
}

function hidePass() {
  if (state.passTimer) { clearTimeout(state.passTimer); state.passTimer = null; }
  els.pass.hidden = true;
  els.pass.classList.remove('bad');
  els.passAction.hidden = true;
}

// currentInstruction names the step the subject was on, for a failure message
// that says which one rather than only that something went wrong.
function currentInstruction() {
  return INSTRUCTIONS[state.currentChallenge]?.text || 'Langkah ini';
}

// api calls the service.
//
// No API key anywhere in this file, deliberately. A key is an operator's
// credential and the person in front of the camera is a subject; putting one in
// their browser is how it ends up somewhere it should not be. What authorises
// these calls is the session's own nonce, which the server hands back when the
// session opens.
async function api(path, options = {}) {
  const headers = {
    'Content-Type': 'application/json',
    ...(options.headers || {}),
  };
  if (state.session?.nonce) headers['X-Session-Nonce'] = state.session.nonce;

  const res = await fetch(path, { ...options, headers });

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

  // Downscale, but not below what the server measures against.
  //
  // The old target of 480 was justified by "the detector letterboxes to 320
  // anyway", which is true and beside the point: the server rejects any face
  // narrower than the embedder's own 112 px input, measured in the pixels it
  // was sent. A face at ordinary laptop distance came out 105-111 px wide at
  // 480 — one to seven pixels short — so every frame of a session was refused
  // before a single challenge was ever evaluated.
  //
  // 720 puts that same face near 160 px. The detector still letterboxes to
  // 320; the extra pixels are read by the stages after it, which crop from the
  // frame at full resolution.
  const target = 720;
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
      // The last step earns the same confirmation as the others; the verdict
      // badge that follows says the session as a whole passed, which is a
      // different statement.
      showPass(state.currentChallenge, null);
      await finish();
      return;
    }

    // Captured before showChallenge, which is what rotates currentChallenge on
    // to the next one.
    const finished = res.advanced ? state.currentChallenge : null;

    if (res.challenge) showChallenge(res.challenge, res.seconds_remaining);
    else syncCountdown(res.seconds_remaining);

    if (res.retried) {
      showRetry(res.challenge, res.retries_left);
      setStatus('Waktu langkah ini habis. Mengulang langkah yang sama.', 'warn');
      state.lastReason = '';
    } else if (res.advanced) {
      showPass(finished, res.challenge);
      setStatus('Bagus.', 'ok');
      state.lastReason = '';
    } else if (res.reason) {
      setStatus(res.reason);
      state.lastReason = res.reason;
    } else {
      setStatus('');
      state.lastReason = '';
    }
  } catch (err) {
    // 410 is a deadline that ran out; 422 is a verification failure.
    if (err.status === 410) {
      stop();
      showFail(`${currentInstruction()} — belum selesai`,
        'Waktu untuk langkah ini habis, jadi seluruh sesi harus diulang dari awal.');
      setBadge('WAKTU HABIS', 'bad');
      setStatus('Waktu untuk langkah ini habis. Mulai lagi dari awal.', 'bad');
      return;
    }
    if (err.status === 422) {
      stop();
      showFail('Verifikasi gagal',
        'Sesi ini dihentikan dan harus diulang dari awal.');
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
      state.done = new Set(state.session.challenges);
      state.currentChallenge = null;
      renderSteps();

      showResult(true, 'Verifikasi berhasil',
        'Semua langkah selesai. Anda terverifikasi sebagai orang sungguhan.', 'Mulai lagi');
      setBadge('LOLOS', 'ok');
      setStatus('Verifikasi berhasil.', 'ok');
    } else {
      showFail('Verifikasi gagal', `Sesi berakhir dengan status ${verdict.state}.`);
      setBadge('GAGAL', 'bad');
      setStatus(`Sesi berakhir dengan status ${verdict.state}.`, 'bad');
    }
  } catch (err) {
    showFail('Verifikasi gagal', err.message);
    setBadge('GAGAL', 'bad');
    setStatus(err.message, 'bad');
  }
  stop();
}

function stopCapture() {
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
  if (state.ticker) { clearInterval(state.ticker); state.ticker = null; }
  els.timer.hidden = true;
  els.timer.classList.remove('warn', 'critical');
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
}

async function start() {
  setBadge(null);
  hidePass();
  setStatus('Meminta akses kamera…');
  els.start.disabled = true;

  try {
    state.stream = await navigator.mediaDevices.getUserMedia({
      // 720p rather than 480p: at 640x480 a face at ordinary laptop distance
      // is around 143 px wide before the send-side downscale, which leaves no
      // margin over the server's 112 px floor once anything is resized.
      video: { facingMode: 'user', width: { ideal: 1280 }, height: { ideal: 720 } },
      audio: false,
    });
  } catch (err) {
    els.start.disabled = false;
    if (err.name === 'NotAllowedError') {
      setStatus('Akses kamera ditolak. Izinkan lewat ikon di bilah alamat, lalu coba lagi.', 'bad');
    } else if (!window.isSecureContext) {
      setStatus('Kamera hanya bisa diakses lewat localhost atau HTTPS. Buka http://localhost:8080/demo/.', 'bad');
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
    if (err.status === 401) {
      // Anonymous session creation is off, which is the safe default. This page
      // has no backend to hold a key, so it cannot open a session on its own.
      setStatus('Server ini tidak mengizinkan sesi anonim. Setel LV_ALLOW_ANONYMOUS_SESSIONS=true ' +
        'untuk demo, atau buat sesi dari backend Anda sendiri.', 'bad');
    } else if (err.status === 429) {
      setStatus('Terlalu banyak percobaan. Tunggu sebentar lalu coba lagi.', 'warn');
    } else {
      setStatus(err.message, 'bad');
    }
    return;
  }

  state.seq = 0;
  state.done = new Set();
  state.currentChallenge = null;
  state.challengeSeconds = state.session.challenge_seconds || 0;

  showChallenge(state.session.challenges[0], state.session.seconds_remaining);

  els.start.hidden = true;
  els.stop.hidden = false;
  setStatus('Ikuti instruksi.');

  state.timer = setInterval(sendFrame, FRAME_INTERVAL_MS);
  state.ticker = setInterval(drawCountdown, TICK_MS);
}

els.start.addEventListener('click', start);
els.stop.addEventListener('click', () => {
  hidePass();
  stop();
  setStatus('Dibatalkan.');
  setBadge(null);
});

// The verdict overlay covers the whole viewport, so its button is the only way
// out of it. It restarts rather than merely dismissing: after a verdict there is
// nothing behind the overlay to go back to.
els.passAction.addEventListener('click', () => {
  hidePass();
  start();
});

// A quick honesty check on load: tell the operator whether the models are
// actually in use, because a stub session proves only that the wiring works.
fetch('/healthz')
  .then((r) => r.json())
  .then((body) => { els.mode.textContent = body.pipeline || 'tidak diketahui'; })
  .catch(() => { els.mode.textContent = 'tidak dapat dihubungi'; });

if (!window.isSecureContext) {
  setStatus('Halaman ini bukan secure context; kamera tidak akan bisa diakses. Gunakan http://localhost:8080/demo/.', 'warn');
}
