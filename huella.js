/* HUELLA r1 - the page's side of a macro recorder that keeps WHAT each click landed on as well as WHERE.

   The program (huella-r1.exe) is the camera, the ears and the hands. This script is added to CADENA LINCE r3 as the
   program serves it - the LINCE file itself is not touched - and it is the reader:

     the loop   one request is always open to the program. Whenever the screen has changed, the new frame comes back,
                LINCE reads it, and the next is asked for the moment that reading is done. Nothing waits on a timer,
                so it keeps eating with the window covered or the tab in the background.
     a click    the program names the frame taken just BEFORE the click. If that frame was already read, the answer
                is there at once; if not, the frame comes along and is read now. One question is answered:
                was that a zone I recognise? The answer is kept with the click as its WHAT; x,y stay as its WHERE.
     a run      for each step the same question is asked the other way round: where does that zone stand on the screen
                as it is now? When it is found, and found in the same place twice (or the screen has stopped changing),
                the program is told to click there. A step that landed on nothing recognised goes back to its WHERE.

   Every x,y is a real pixel of the primary screen: frame = capture. */
(function () {
  'use strict';
  var HU = window.HUELLA || {}, LINCE = window.LINCE;
  if (!LINCE || !HU.token) return;

  var CLIENT = Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
  var CAP = { frame: 'capture' };
  var WAIT_MS = 8000;        /* how long a step waits for its target to show up before the run gives in */
  var NEAR = 0.06;           /* a click on nothing is tied to print standing within this much of the screen's height */
  var COUNTDOWN = 3;         /* seconds, when PLAY is pressed on this page: time to put the right window in front */
  var GAP = [350, 1500];     /* the pause after a click, taken from the recording but kept between these (ms) */
  var COL = LINCE.colors;

  var st = { mode: 'idle', seq: -1, steps: [], screen: [0, 0], play: { tok: 0, delay: 0 }, last: {}, file: '' };
  var cache = [];            /* the last three frames read: { id, res } */
  var lastId = 0, view = null, painted = null, off = null;
  var mark = null, markN = 0; /* the last click answered or made, for the picture */
  var run = null;            /* the run in hand */
  var ran = { tok: -1, did: {}, fail: -1, msg: '' };
  var stats = { n: 0, ms: 0, zones: 0, times: [] };
  var gone = false, note = '';

  var $ = function (id) { return document.getElementById(id); };
  function el(tag, cls, txt) { var e = document.createElement(tag); if (cls) e.className = cls; if (txt != null) e.textContent = txt; return e; }
  function clamp(v, lo, hi) { return v < lo ? lo : v > hi ? hi : v; }
  function api(path, body) {
    var o = { headers: { 'X-Huella': HU.token }, cache: 'no-store' };
    if (body !== undefined) { o.method = 'POST'; o.body = JSON.stringify(body); }
    return fetch(path, o);
  }

  /* ------------------------------------------------------------ reading ------------------------------------------------------------ */

  /* the camera hands over GDI's own bytes (blue first); LINCE wants red first. Swapped in place, alpha made solid. */
  function read(buf, W, H, id) {
    var p = new Uint32Array(buf, 0, W * H);
    for (var i = 0; i < p.length; i++) { var v = p[i]; p[i] = 0xFF000000 | (v & 0xFF00) | ((v & 0xFF) << 16) | ((v >>> 16) & 0xFF); }
    var t0 = performance.now();
    var res = LINCE.analyze(new Uint8Array(buf), W, H, {});
    var t1 = performance.now();
    stats.n++; stats.ms = Math.round(t1 - t0); stats.zones = res.zones.length;
    stats.times.push(t1); if (stats.times.length > 6) stats.times.shift();
    cache.push({ id: id, res: res }); if (cache.length > 3) cache.shift();
    if (id > lastId) { lastId = id; view = res; }
    return res;
  }
  function cached(id) { for (var i = 0; i < cache.length; i++) if (cache[i].id === id) return cache[i].res; return null; }

  function squash(s) { return String(s).toLowerCase().replace(/[^a-z0-9]+/g, ''); }
  function lev(a, b) {
    var m = a.length, n = b.length, prev = [], cur = [], i, j, t;
    for (j = 0; j <= n; j++) prev[j] = j;
    for (i = 1; i <= m; i++) { cur[0] = i; for (j = 1; j <= n; j++) cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a.charCodeAt(i - 1) === b.charCodeAt(j - 1) ? 0 : 1)); t = prev; prev = cur; cur = t; }
    return prev[n];
  }
  function mid(b) { return [(b[0] + b[2] + 1) / 2, (b[1] + b[3] + 1) / 2]; }

  /* every place this print stands on a frame, in LINCE's reading order. A zone that is exactly these words comes before
     the same words inside a longer phrase; a read that is one letter off is taken only when nothing reads true. */
  function places(res, text, whole) {
    var q = squash(text), all = LINCE.find(res, text, CAP), a = [], b = [], c = [];
    for (var i = 0; i < all.length; i++) {
      var m = all[i], s = squash(m.text);
      if (s === q) { if (squash(m.zoneText) === q) a.push(m); else b.push(m); }
      else if (q.length >= 5 && Math.abs(s.length - q.length) <= 1 && lev(s, q) <= Math.floor(q.length / 5)) c.push(m);
    }
    var exact = whole ? (a.length ? a : b) : a.concat(b), list = exact.length ? exact : c;
    list.sort(function (p, r) { return (p.id - r.id) || (p.px[0] - r.px[0]); });
    return { list: list, nearly: !exact.length && c.length > 0 };
  }

  /* a zone of print as something to find again: its words (three of them, where it is a long line), where they stand,
     and how far from their middle the click fell */
  function target(res, z, x, y, kind) {
    var ws = z.words && z.words.length ? z.words : null, text = z.text, whole = true, i;
    if (ws && ws.length > 6) {
      var k = 0, kd = Infinity;
      for (i = 0; i < ws.length; i++) { var d = Math.max(0, ws[i].x0 - x, x - ws[i].x1); if (d < kd) { kd = d; k = i; } }
      var a = clamp(k - 1, 0, ws.length - 3);
      text = ws.slice(a, a + 3).map(function (w) { return w.t; }).join(' '); whole = false;
    }
    if (!squash(text)) return null;
    var c = places(res, text, whole).list, nth = -1;
    for (i = 0; i < c.length; i++) if (c[i].id === z.id) { nth = i; break; }
    if (nth < 0) return null;
    var b = c[nth].px, m = mid(b);
    var t = { kind: kind, type: z.type, text: text, whole: whole, dx: Math.round(x - m[0]), dy: Math.round(y - m[1]), box: b, nth: nth, of: c.length };
    if (!whole) t.phrase = z.text;
    return t;
  }
  /* an icon, a swatch, a tick box: found again by what LINCE says it stands beside */
  function sameNear(res, type, near) { return res.zones.filter(function (q) { return q.type === type && q.near === near; }); }
  function pictureTarget(res, z, x, y) {
    var l = sameNear(res, z.type, z.near), b = [z.x0, z.y0, z.x1, z.y1], m = mid(b);
    return { kind: 'on', type: z.type, near: z.near, dx: Math.round(x - m[0]), dy: Math.round(y - m[1]), box: b, nth: l.indexOf(z), of: l.length };
  }

  /* the one question: what stood where this click landed? */
  function whatAt(res, x, y) {
    var zs = res.zones, on = null, oa = Infinity, i, z;
    for (i = 0; i < zs.length; i++) { z = zs[i]; if (x >= z.x0 && x <= z.x1 && y >= z.y0 && y <= z.y1 && z.w * z.h < oa) { oa = z.w * z.h; on = z; } }
    var what = null, alt = null;
    if (on && on.text) what = target(res, on, x, y, 'on');
    else if (on && on.near && on.type !== 'image') what = pictureTarget(res, on, x, y);
    /* the nearest print: all there is to go by when the click landed on nothing known, a second way to an icon otherwise */
    if (!what || !what.text) {
      var best = null, bd = Math.max(40, NEAR * res.H);
      for (i = 0; i < zs.length; i++) {
        z = zs[i]; if (z === on || !z.text || !squash(z.text)) continue;
        var dx = Math.max(0, z.x0 - x, x - z.x1), dy = Math.max(0, z.y0 - y, y - z.y1), d = Math.sqrt(dx * dx + dy * dy);
        if (d < bd) { bd = d; best = z; }
      }
      var a = best ? target(res, best, x, y, 'near') : null;
      if (!what) what = a; else alt = a;
    }
    return { what: what, alt: alt };
  }

  /* the same question the other way round: where is that thing on this frame? */
  function locate(w, res) {
    var list, nearly = false;
    if (w.text) { var c = places(res, w.text, w.whole); list = c.list.map(function (m) { return m.px; }); nearly = c.nearly; }
    else if (w.near) list = sameNear(res, w.type, w.near).map(function (q) { return [q.x0, q.y0, q.x1, q.y1]; });
    else return null;
    if (!list.length) return null;
    var b = null;
    if (list.length === w.of && w.nth >= 0 && w.nth < list.length) b = list[w.nth];      /* as many as there were: the same one of them */
    else { var o = mid(w.box), bd = Infinity; list.forEach(function (p) { var m = mid(p), d = Math.abs(m[0] - o[0]) + Math.abs(m[1] - o[1]); if (d < bd) { bd = d; b = p; } }); }      /* else the one nearest to where it was */
    var m2 = mid(b);
    return { x: clamp(Math.round(m2[0] + w.dx), 0, res.W - 1), y: clamp(Math.round(m2[1] + w.dy), 0, res.H - 1), box: b, how: (nearly ? 'nearly ' : '') + w.kind };
  }
  function aim(step, res) {
    if (!step.what && !step.alt) return { x: step.x, y: step.y, box: null, how: 'at' };
    return (step.what && locate(step.what, res)) || (step.alt && locate(step.alt, res)) || null;
  }

  /* ------------------------------------------------------------ the loop ------------------------------------------------------------ */

  async function sync() {
    var r = await api('/api/state'), s = await r.json();
    st = s;
    if (st.mode !== 'play') run = null;
    if (st.mode === 'rec') { ran.tok = -1; ran.did = {}; ran.fail = -1; ran.msg = ''; }
    draw();
  }

  async function answer(id, x, y, res) {
    var a = res ? whatAt(res, x, y) : { what: null, alt: null };
    mark = { x: x, y: y, box: a.what ? a.what.box : null }; markN++;
    await api('/api/what', { id: id, what: a.what, alt: a.alt });
  }

  async function finish(ok, msg, at) {
    var tok = run.tok;
    ran.msg = msg; ran.fail = ok ? -1 : at;
    run = null;
    await api('/api/done', { tok: tok, ok: ok, msg: msg, step: at == null ? -1 : at });
  }

  async function begin() {
    run = { tok: st.play.tok, i: 0, t0: 0, prev: null, prevId: -1 };
    ran = { tok: run.tok, did: {}, fail: -1, msg: '' };
    mark = null; markN++;
    for (var s = Math.round((st.play.delay || 0) / 1000); s > 0; s--) {
      note = 'STARTS IN ' + s + ' — PUT THE RIGHT WINDOW IN FRONT'; draw();
      var r = await api('/api/wait?ms=1000&tok=' + run.tok);
      if (!r.ok) { run = null; note = ''; return; }
    }
    note = '';
  }

  /* one look at the screen during a run. kind 'same' means the camera saw no change while we waited. */
  async function playTick(kind) {
    if (!run || run.tok !== st.play.tok) { await begin(); if (!run) return; }
    var step = st.steps[run.i];
    if (!step) return finish(true, 'RAN ALL ' + st.steps.length + ' STEPS');
    var res = view;
    if (!res) return;                                       /* the camera has not handed over a frame yet */
    if (!run.t0) run.t0 = performance.now();
    var p = aim(step, res);
    if (!p) {
      run.prev = null;
      if (performance.now() - run.t0 > WAIT_MS) return finish(false, 'STEP ' + (run.i + 1) + ' NOT FOUND ON SCREEN — STOPPED THERE', run.i);
      return;
    }
    var steady = p.how === 'at' || kind === 'same' || (run.prev && run.prevId !== lastId && Math.abs(run.prev.x - p.x) <= 2 && Math.abs(run.prev.y - p.y) <= 2);
    run.prev = p; run.prevId = lastId;
    if (!steady) return;                                    /* seen once: look again before trusting it */
    var r = await api('/api/click', { x: p.x, y: p.y, button: step.button, count: step.count, tok: run.tok });
    if (!r.ok) { run = null; return; }
    mark = { x: p.x, y: p.y, box: p.box }; markN++;
    ran.did[run.i] = p.how;
    run.i++; run.t0 = 0; run.prev = null;
    var next = st.steps[run.i];
    if (!next) return finish(true, 'RAN ALL ' + st.steps.length + ' STEPS');
    draw();
    var w = await api('/api/wait?ms=' + clamp(next.gap || 0, GAP[0], GAP[1]) + '&tok=' + run.tok);
    if (!w.ok) run = null;
  }

  async function loop() {
    for (;;) {
      var r;
      try { r = await api('/api/next?c=' + CLIENT + '&after=' + lastId + '&seq=' + st.seq + '&wait=' + (st.mode === 'play' ? 250 : 25000)); }
      catch (e) { if (!gone) { gone = true; draw(); } await new Promise(function (ok) { setTimeout(ok, 1000); }); continue; }
      if (r.status === 403) { dead('THIS TAB BELONGS TO AN EARLIER RUN OF THE PROGRAM — CLOSE IT AND USE THE ONE THE PROGRAM JUST OPENED'); return; }
      gone = false;
      var kind = r.headers.get('X-Kind');
      if (kind === 'retired') { dead('A NEWER TAB TOOK OVER — THIS ONE HAS STOPPED'); return; }
      try {
        var res = null, id = +r.headers.get('X-Frame') || 0;
        if (kind === 'frame' || kind === 'click') res = r.headers.get('X-Body') === '1' ? read(await r.arrayBuffer(), +r.headers.get('X-W'), +r.headers.get('X-H'), id) : cached(id);
        if (+r.headers.get('X-Seq') !== st.seq) await sync();
        if (kind === 'click') await answer(+r.headers.get('X-Step'), +r.headers.get('X-X'), +r.headers.get('X-Y'), res);
        if (st.mode === 'play') await playTick(kind);
      } catch (e) { console.error(e); note = 'ERROR: ' + (e && e.message || e); }
      draw();
    }
  }

  /* ------------------------------------------------------------ the panel ------------------------------------------------------------ */

  var css = document.createElement('style');
  css.textContent = [
    '#huStrip{display:flex;flex-wrap:wrap;justify-content:space-between;gap:4px 14px;font-size:12px;font-weight:700;letter-spacing:.06em;color:var(--sec);line-height:1.4;min-height:20px;margin-bottom:10px}',
    '#huStrip b{color:var(--ink)}',
    '#huView{border:1px solid var(--line);background:var(--inset);line-height:0;margin-bottom:10px}',
    '#huCv{display:block;width:100%;height:auto}',
    '#huSteps{border-top:2px solid var(--heavy);margin-bottom:10px}',
    '.hu-step{display:flex;gap:10px;align-items:baseline;padding:10px 6px;border-bottom:1px solid var(--line);border-left:6px solid transparent;min-height:56px}',
    '.hu-step.now{border-left-color:var(--ink);background:var(--inset)}',
    '.hu-n{font-weight:700;min-width:1.8em;text-align:right;font-variant-numeric:tabular-nums}',
    '.hu-b{flex:1 1 auto;min-width:0;font-size:15px;line-height:1.45;word-break:break-word}',
    '.hu-k{display:block;color:var(--sec);font-size:12px;letter-spacing:.04em;margin-top:2px}',
    '.hu-t{display:inline-block;font-size:11px;font-weight:700;letter-spacing:.08em;color:#FFFFFF;padding:2px 6px;margin-right:8px}',
    '.hu-t.on{background:var(--ok)}.hu-t.near{background:var(--amber)}.hu-t.at{background:var(--meta)}.hu-t.rd{background:var(--blue)}',
    '.hu-r{font-size:11px;font-weight:700;letter-spacing:.08em;white-space:nowrap}.hu-r.ok{color:var(--ok)}.hu-r.no{color:var(--err)}',
    '#huRec[aria-pressed="true"]{background:var(--err);border-color:var(--err);color:#FFFFFF}',
    '@keyframes huPulse{50%{opacity:.25}}',
    '#huDot{display:inline-block;width:10px;height:10px;background:var(--meta);margin-right:7px}',
    '.hu-rec #huDot{background:var(--err);animation:huPulse 1s steps(2,jump-none) infinite}.hu-play #huDot{background:var(--ok);animation:huPulse 1s steps(2,jump-none) infinite}'
  ].join('\n');
  document.head.appendChild(css);

  var panel = el('div', 'panel'); panel.id = 'hu';
  var hd = el('div', 'hd'); hd.appendChild(document.createTextNode('HUELLA ' + (HU.rev || '') + ' · WHAT AND WHERE ')); var hdR = el('span', 'r'); hdR.id = 'huMeta'; hd.appendChild(hdR); panel.appendChild(hd);
  var bd = el('div', 'bd'); panel.appendChild(bd);
  var row = el('div', 'row');
  function button(id, label, sub) { var b = el('button'); b.type = 'button'; b.id = id; b.appendChild(el('span', '', label)); if (sub) b.appendChild(el('span', 'sub', sub)); row.appendChild(b); return b; }
  var bRec = button('huRec', 'RECORD', 'F9'), bPlay = button('huPlay', 'PLAY', 'F8 · FROM HERE IT STARTS IN ' + COUNTDOWN + ' S'), bStop = button('huStop', 'STOP', 'ESC'), bCopy = button('huCopy', 'COPY MACRO', 'JSON'), bClear = button('huClear', 'CLEAR', '');
  bd.appendChild(row);
  var strip = el('div'); strip.id = 'huStrip'; var sL = el('span'), sR = el('span'), sT = el('span'), dot = el('span'); dot.id = 'huDot';
  sL.appendChild(dot); sL.appendChild(sT); strip.appendChild(sL); strip.appendChild(sR); bd.appendChild(strip);
  var stepsBox = el('div'); stepsBox.id = 'huSteps'; bd.appendChild(stepsBox);
  var vw = el('div'); vw.id = 'huView'; var cv = el('canvas'); cv.id = 'huCv'; cv.width = 16; cv.height = 9; vw.appendChild(cv); bd.appendChild(vw);
  var help = el('div', 'note'); bd.appendChild(help);
  help.appendChild(el('p', '', 'RECORD (or F9 from anywhere), click through the job in any window, F9 again. Each click is kept as WHAT it landed on — read by LINCE off the last frame the camera took before the click — and WHERE. A click on nothing LINCE knows is tied to the nearest print with its offset, or, with none close by, kept as a bare point.'));
  help.appendChild(el('p', '', 'PLAY (or F8) looks for each WHAT on the screen as it is now and clicks it there, wherever its window has moved to; it waits up to ' + (WAIT_MS / 1000) + ' s for a thing to show up and stops if it does not. Esc stops a run. Clicks on this page are never recorded. Primary screen only; no typing yet.'));
  var fileNote = el('p', ''); help.appendChild(fileNote);
  var host = $('zone'); host.parentNode.insertBefore(panel, host);
  document.title = (HU.mark || 'HUELLA') + ' ' + (HU.rev || '') + ' · ' + document.title;

  bRec.addEventListener('click', function () { api('/api/rec?on=' + (st.mode === 'rec' ? 0 : 1), null); });
  bPlay.addEventListener('click', function () { api('/api/play?on=1&delay=' + COUNTDOWN * 1000, null); });
  bStop.addEventListener('click', function () { api('/api/stop', null); });
  bCopy.addEventListener('click', function () {
    api('/api/macro').then(function (r) { return r.text(); }).then(function (t) { return navigator.clipboard.writeText(t); }).then(function () { flash(bCopy, 'COPIED'); }, function () { flash(bCopy, 'COULD NOT COPY'); });
  });
  var armed = 0;
  bClear.addEventListener('click', function () {                       /* two taps to throw the steps away */
    if (Date.now() - armed < 4000) { armed = 0; api('/api/clear', null); draw(); return; }
    armed = Date.now(); draw(); setTimeout(draw, 4100);
  });
  function flash(b, word) { var s = b.firstChild, t = s.textContent; s.textContent = word; setTimeout(function () { s.textContent = t; }, 1000); }
  function dead(msg) { gone = true; note = msg; [bRec, bPlay, bStop, bClear, bCopy].forEach(function (b) { b.disabled = true; }); draw(); }

  function sgn(v) { return (v < 0 ? '−' : '+') + Math.abs(v); }
  function clip(s, n) { return s.length > n ? s.slice(0, n - 1) + '…' : s; }
  function say(w) {
    var s = w.text ? '"' + w.text + '"' : w.type + ' ' + w.near;
    if (w.phrase) s += '  in  "' + clip(w.phrase, 70) + '"';
    if (w.kind === 'near') s += '  ' + sgn(w.dx) + ', ' + sgn(w.dy);
    if (w.of > 1) s += '  · ' + (w.nth + 1) + ' OF ' + w.of + ' ON SCREEN';
    return s;
  }
  function draw() {
    var n = st.steps.length, m = st.mode;
    document.documentElement.classList.toggle('hu-rec', m === 'rec'); document.documentElement.classList.toggle('hu-play', m === 'play');
    document.documentElement.dataset.huella = m;
    bRec.setAttribute('aria-pressed', m === 'rec' ? 'true' : 'false'); bRec.firstChild.textContent = m === 'rec' ? 'RECORDING' : 'RECORD';
    bRec.disabled = gone || m === 'play'; bPlay.disabled = gone || m !== 'idle' || !n; bStop.disabled = gone || m === 'idle'; bClear.disabled = gone || m !== 'idle' || !n; bCopy.disabled = gone || !n;
    var hot = Date.now() - armed < 4000 && !bClear.disabled;
    bClear.classList.toggle('no', hot); bClear.firstChild.textContent = hot ? 'TAP AGAIN TO CLEAR ' + n + ' STEP' + (n === 1 ? '' : 'S') : 'CLEAR';
    hdR.textContent = (m === 'rec' ? 'RECORDING' : m === 'play' ? 'PLAYING' : 'IDLE') + ' · ' + n + ' STEP' + (n === 1 ? '' : 'S');
    /* the strip: what the reader is doing */
    var rate = stats.times.length > 1 ? (stats.times.length - 1) / ((stats.times[stats.times.length - 1] - stats.times[0]) / 1000) : 0;
    sT.textContent = gone && !note ? 'THE PROGRAM IS NOT ANSWERING — IS IT STILL RUNNING?' : note ? note : st.camera === 'no picture' ? 'THE CAMERA GETS NO PICTURE OF THE SCREEN' : m === 'idle' ? (ran.msg || (st.last && st.last.msg) || 'IDLE — THE CAMERA IS OFF') : (m === 'rec' ? 'EATING EVERY NEW FRAME' : 'STEP ' + (run ? run.i + 1 : 1) + ' OF ' + n);
    sR.textContent = stats.n ? st.screen[0] + ' × ' + st.screen[1] + ' · ' + stats.n + ' FRAMES READ · ' + stats.zones + ' ZONES · ' + stats.ms + ' MS' + (rate ? ' · ' + rate.toFixed(1) + ' / S' : '') : st.screen[0] + ' × ' + st.screen[1];
    fileNote.textContent = st.file ? 'The macro is also kept as a file: ' + st.file : '';
    /* the steps */
    stepsBox.textContent = '';
    if (!n) stepsBox.appendChild(el('div', 'hu-step', 'NO STEPS YET'));
    st.steps.forEach(function (s, i) {
      var r = el('div', 'hu-step' + (run && run.i === i ? ' now' : ''));
      r.appendChild(el('div', 'hu-n', String(i + 1)));
      var b = el('div', 'hu-b'), w = s.what;
      if (!s.read) b.appendChild(el('span', 'hu-t rd', 'READING'));
      else if (!w) { b.appendChild(el('span', 'hu-t at', 'AT')); b.appendChild(document.createTextNode(s.x + ', ' + s.y + '  —  nothing recognised there: the point itself')); }
      else { b.appendChild(el('span', 'hu-t ' + w.kind, w.kind.toUpperCase())); b.appendChild(document.createTextNode(say(w))); }
      b.appendChild(el('span', 'hu-k', [w && w.type ? w.type.toUpperCase() : '', (s.button + ' click' + (s.count > 1 ? ' ×' + s.count : '')).toUpperCase(), s.x + ', ' + s.y, s.win ? clip(s.win, 60) : ''].filter(Boolean).join(' · ')));
      r.appendChild(b);
      if (ran.tok >= 0 && ran.did[i]) r.appendChild(el('div', 'hu-r ok', 'CLICKED ' + ran.did[i].toUpperCase()));
      else if (ran.tok >= 0 && ran.fail === i) r.appendChild(el('div', 'hu-r no', 'NOT FOUND'));
      stepsBox.appendChild(r);
    });
    paint();
  }

  /* the last frame read, with what LINCE made of it and where the last click fell. Only drawn while someone can see it. */
  function paint() {
    var res = view, key = lastId + ':' + markN;
    if (document.hidden || !res || !res.rgba || painted === key) return;
    painted = key;
    var W = res.W, H = res.H, cw = Math.min(W, Math.round((vw.clientWidth || 600) * (window.devicePixelRatio || 1))), ch = Math.round(cw * H / W), s = cw / W;
    if (!off || off.width !== W || off.height !== H) { off = document.createElement('canvas'); off.width = W; off.height = H; }
    off.getContext('2d').putImageData(new ImageData(new Uint8ClampedArray(res.rgba.buffer, res.rgba.byteOffset, W * H * 4), W, H), 0, 0);
    cv.width = cw; cv.height = ch;
    var g = cv.getContext('2d'); g.imageSmoothingQuality = 'high'; g.drawImage(off, 0, 0, W, H, 0, 0, cw, ch);
    g.lineWidth = 1;
    for (var i = 0; i < res.zones.length; i++) { var z = res.zones[i]; g.strokeStyle = COL[z.type] || '#555555'; g.strokeRect(Math.round(z.x0 * s) + 0.5, Math.round(z.y0 * s) + 0.5, Math.max(1, Math.round(z.w * s)), Math.max(1, Math.round(z.h * s))); }
    if (mark) {
      var x = mark.x * s, y = mark.y * s, r = 14;
      if (mark.box) { g.strokeStyle = '#1A1A1A'; g.lineWidth = 3; g.strokeRect(mark.box[0] * s - 3, mark.box[1] * s - 3, (mark.box[2] - mark.box[0] + 1) * s + 6, (mark.box[3] - mark.box[1] + 1) * s + 6); }
      g.strokeStyle = '#FFFFFF'; g.lineWidth = 5; g.beginPath(); g.moveTo(x - r, y); g.lineTo(x + r, y); g.moveTo(x, y - r); g.lineTo(x, y + r); g.stroke();
      g.strokeStyle = '#B3261E'; g.lineWidth = 2; g.beginPath(); g.moveTo(x - r, y); g.lineTo(x + r, y); g.moveTo(x, y - r); g.lineTo(x, y + r); g.stroke();
    }
  }
  document.addEventListener('visibilitychange', function () { if (!document.hidden) { painted = null; draw(); } });

  /* a tab that holds a lock is not put to sleep by the browser while it waits for the camera */
  try { if (navigator.locks) navigator.locks.request('huella-' + CLIENT, function () { return new Promise(function () {}); }); } catch (e) {}

  window.HUELLA.whatAt = whatAt; window.HUELLA.aim = aim; window.HUELLA.state = function () { return st; };
  draw();
  loop();
})();
