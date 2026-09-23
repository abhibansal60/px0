// web/src/terminal.js
import { $, S, apiPostJson, isMac, keyLabel, esc } from './state.js';
import { showToast } from './ui.js';
import { layout, render } from './renderer.js';
import { setHandoffHandler, selectionRef } from './selbar.js';

/* The terminal pane: Shell and Harness Sessions hosted by the px0 process
   (terminal.go), drawn with xterm.js. The server owns every session and its
   scrollback, so this module is a view: it can be reloaded, or opened in a
   second tab, and pick up where the sessions are.

   xterm.js is a vendored static file, loaded the first time the pane is needed
   rather than bundled, so px0's startup and memory are unchanged for anyone
   who never opens a terminal.

   All sessions share one SSE stream (/api/term/stream). Each output frame
   carries the byte offsets reached per session as its SSE id; EventSource sends
   it back on reconnect, and reopening the stream passes it as ?since=, so a
   page resumes exactly where it stopped. A frame marked reset means earlier
   bytes are gone (a fresh page, or one that fell behind the server's ring
   buffer): clear, then write. */

const pane = $('#termpane');
const resizer = $('#term-resizer');
const tabsEl = $('#term-tabs');
const bodyEl = $('#term-body');
const noteEl = $('#term-note');
const newHarnessBtn = $('#term-new-harness');
const newShellBtn = $('#term-new-shell');
const footerBtn = $('#footer-actions [data-action="terminal"]');

const termSessions = new Map();   // id -> { info, term, fit, host, exitShown }
let active = null;            // id of the session shown
let lastHarness = null;       // harness session most recently focused: Alt+E's target
let stream = null;            // EventSource
let cursors = '';             // last SSE id: where to resume from
let xtermLoad = null;         // promise of the vendored xterm.js

const termMeta = () => S.meta?.terminal || {};
const usable = () => !!(termMeta().available && termMeta().authorized);
const store = (k, v) => { try { v === undefined ? localStorage.removeItem(k) : localStorage.setItem(k, v); } catch {} };
const recall = k => { try { return localStorage.getItem(k); } catch { return null; } };

function loadXterm() {
  if (xtermLoad) return xtermLoad;
  const url = p => new URL('static/vendor/xterm/' + p, document.baseURI).href;
  const add = (tag, attrs) => new Promise((ok, fail) => {
    const el = Object.assign(document.createElement(tag), attrs);
    el.onload = ok;
    el.onerror = () => fail(new Error('could not load ' + (attrs.src || attrs.href)));
    document.head.append(el);
  });
  xtermLoad = Promise.all([
    add('link', { rel: 'stylesheet', href: url('xterm.css') }),
    add('script', { src: url('xterm.js') }).then(() => add('script', { src: url('addon-fit.js') })),
  ]).catch(e => { xtermLoad = null; throw e; });
  return xtermLoad;
}

/* Colours follow the px0 theme: the ANSI palette is mapped from its syntax and
   diff tokens so terminal output reads like the code around it. */
function xtermTheme() {
  const cs = getComputedStyle(document.documentElement);
  const v = (name, fallback) => cs.getPropertyValue(name).trim() || fallback;
  const fg = v('--fg', '#ddd'), bg = v('--bg', '#111');
  const ansi = {
    black: v('--faint', '#666'), red: v('--gd', '#e06c75'), green: v('--gi', '#98c379'), yellow: v('--mark-active', '#e5c07b'),
    blue: v('--nf', '#61afef'), magenta: v('--k', '#c678dd'), cyan: v('--o', '#56b6c2'), white: v('--dim', '#abb2bf'),
  };
  const bright = Object.fromEntries(Object.entries(ansi).map(([k, c]) => ['bright' + k[0].toUpperCase() + k.slice(1), c]));
  return { background: bg, foreground: fg, cursor: fg, cursorAccent: bg, selectionBackground: v('--sel', '#444'), ...ansi, ...bright, brightWhite: fg };
}

function editorFont() {
  const cs = getComputedStyle(document.documentElement);
  return cs.getPropertyValue('--mono').trim() || 'monospace';
}

function b64bytes(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/* ---------- input: one request in flight, so keystrokes arrive in order ---------- */

const outbox = [];
let sending = false;

function send(id, data, bin = false) {
  const last = outbox[outbox.length - 1];
  if (last && last.id === id && last.bin === bin) last.data += data;
  else outbox.push({ id, data, bin });
  if (!sending) flush();
}

async function flush() {
  sending = true;
  while (outbox.length) {
    const item = outbox.shift();
    try {
      await apiPostJson('/api/term/input', item);
    } catch (e) {
      // A session that ended just before the keystroke: nothing to deliver.
      if (!/ended/.test(e.message)) showToast('!', 'Terminal: ' + e.message);
    }
  }
  sending = false;
}

/* ---------- sessions ---------- */

function makeView(info) {
  const host = document.createElement('div');
  host.className = 'term-host';
  host.hidden = true;
  bodyEl.append(host);
  const term = new window.Terminal({
    fontFamily: editorFont(),
    fontSize: 12.5,
    lineHeight: 1.15,
    cursorBlink: true,
    scrollback: 5000,
    allowProposedApi: false,
    macOptionIsMeta: true,
    theme: xtermTheme(),
  });
  const fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);
  term.onData(d => send(info.id, d));
  term.onBinary(d => send(info.id, d, true));
  // Ctrl+Shift+C / V copy and paste on Linux and Windows, where Ctrl+C belongs to the program.
  term.attachCustomKeyEventHandler(e => {
    if (isMac || e.type !== 'keydown' || !e.ctrlKey || !e.shiftKey) return true;
    if (e.code === 'KeyC' && term.hasSelection()) {
      navigator.clipboard?.writeText(term.getSelection()).catch(() => {});
      return false;
    }
    if (e.code === 'KeyV') {
      navigator.clipboard?.readText().then(t => t && term.paste(t)).catch(() => {});
      return false;
    }
    return true;
  });
  term.textarea?.addEventListener('focus', () => {
    if (termSessions.get(info.id)?.info.kind === 'harness') lastHarness = info.id;
  });
  return { info, term, fit, host, exitShown: false, size: '' };
}

// The session list comes from the stream (and /api/meta at boot); it is the truth.
function syncSessions(list) {
  const seen = new Set();
  for (const info of list) {
    seen.add(info.id);
    let s = termSessions.get(info.id);
    if (!s) {
      s = makeView(info);
      termSessions.set(info.id, s);
      if (active == null) active = info.id;
    }
    s.info = info;
    if (info.exited && !s.exitShown) {
      s.exitShown = true;
      s.term.write(`\r\n\x1b[2m[${info.kind === 'harness' ? info.title : 'process'} exited with code ${info.code}]\x1b[0m\r\n`);
    }
  }
  for (const [id, s] of termSessions) {
    if (seen.has(id)) continue;
    s.term.dispose();
    s.host.remove();
    termSessions.delete(id);
    if (lastHarness === id) lastHarness = null;
    if (active === id) active = termSessions.size ? [...termSessions.keys()].pop() : null;
  }
  drawTermTabs();
  showActive();
}

function drawTermTabs() {
  tabsEl.innerHTML = [...termSessions.values()].map(({ info }) =>
    `<button class="term-tab${info.id === active ? ' on' : ''}${info.exited ? ' exited' : ''}" role="tab" data-id="${info.id}" title="${esc(info.title)}${info.exited ? ' (exited ' + info.code + ')' : ''}">` +
    (info.kind === 'harness' ? '<span class="term-kind">AI</span>' : '') +
    `<span class="term-title">${esc(info.title)}</span><span class="term-x" data-close="${info.id}" title="Close session">&times;</span></button>`
  ).join('');
}

function showActive() {
  for (const [id, s] of termSessions) s.host.hidden = id !== active;
  updateNote();
  if (!pane.hidden) fitActive();
}

function updateNote() {
  let msg = '';
  const m = termMeta();
  if (!m.available) msg = esc(m.reason || 'The terminal is turned off.');
  else if (!m.authorized) msg = 'This page was not opened from the URL px0 printed, which carries the terminal\'s access token.<br>Open that URL from the terminal where px0 is running to use the terminal here.';
  else if (!termSessions.size) {
    const h = S.meta?.agent;
    msg = h
      ? `Start <code>${esc(h)}</code> interactively with <b>+ Harness</b>, or a shell with <b>+ Shell</b>.<br>With a harness session open, ${esc(keyLabel('Alt+E'))} on a selection sends its reference here.`
      : 'Start a shell with <b>+ Shell</b>. Pick a coding harness in the footer to start one here too.';
  }
  noteEl.innerHTML = msg && '<div>' + msg + '</div>';
  noteEl.hidden = !msg;
  newShellBtn.disabled = !usable();
  newHarnessBtn.disabled = !usable() || !S.meta?.agent;
  newHarnessBtn.textContent = '+ ' + (S.meta?.agent || 'Harness');
}

// Fit the shown session to the pane and tell its program when the size changed.
function fitActive() {
  const s = termSessions.get(active);
  if (!s || s.host.hidden) return;
  try { s.fit.fit(); } catch { return; }
  const size = s.term.rows + 'x' + s.term.cols;
  if (size === s.size || s.info.exited) return;
  s.size = size;
  apiPostJson('/api/term/resize', { id: s.info.id, rows: s.term.rows, cols: s.term.cols }).catch(() => {});
}

function selectSession(id, focus = true) {
  if (!termSessions.has(id)) return;
  active = id;
  drawTermTabs();
  showActive();
  if (focus) termSessions.get(id).term.focus();
}

// A size for a session that has no view yet: measured from the pane and font.
function estimateSize() {
  const s = termSessions.get(active);
  if (s && s.term.rows) return { rows: s.term.rows, cols: s.term.cols };
  const probe = document.createElement('canvas').getContext('2d');
  probe.font = `12.5px ${editorFont()}`;
  const cw = probe.measureText('M').width || 7.5;
  const r = bodyEl.getBoundingClientRect();
  return { rows: Math.max(5, Math.floor((r.height - 8) / (12.5 * 1.15))), cols: Math.max(20, Math.floor((r.width - 16) / cw)) };
}

async function openSession(kind) {
  if (!usable()) return;
  try {
    await ready();
    showPane(true);
    const res = await apiPostJson('/api/term/open', { kind, ...estimateSize() });
    // The stream announces the session; select it as soon as its view exists.
    const until = Date.now() + 3000;
    while (!termSessions.has(res.id) && Date.now() < until) await new Promise(r => setTimeout(r, 20));
    if (!termSessions.has(res.id)) throw new Error('session started, but the stream has not shown it; reload to attach');
    selectSession(res.id);
    if (kind === 'harness') lastHarness = res.id;
  } catch (e) {
    showToast('!', 'Terminal: ' + e.message);
  }
}

async function closeSession(id) {
  try { await apiPostJson('/api/term/close', { id }); } catch (e) { showToast('!', 'Terminal: ' + e.message); }
}

/* ---------- stream ---------- */

// Connects regardless of visibility: it is called for something the user just
// did. Only the visibilitychange handler below pauses the stream.
function connectStream() {
  if (stream || !usable()) return;
  const u = new URL('api/term/stream', document.baseURI);
  if (cursors) u.searchParams.set('since', cursors);
  stream = new EventSource(u);
  stream.addEventListener('sessions', e => syncSessions(JSON.parse(e.data).sessions || []));
  stream.addEventListener('out', e => {
    cursors = e.lastEventId || cursors;
    const o = JSON.parse(e.data);
    const s = termSessions.get(o.s);
    if (!s) return;
    if (o.r) s.term.reset();
    if (o.d) s.term.write(b64bytes(o.d));
  });
  // EventSource retries by itself, resuming with Last-Event-ID. It gives up
  // only on a non-200 (px0 restarted with a new token): stop there.
  stream.onerror = () => {
    if (stream?.readyState === EventSource.CLOSED) {
      stream = null;
      S.meta.terminal = { ...meta(), authorized: false };
      updateNote();
    }
  };
}

function disconnectStream() {
  stream?.close();
  stream = null;
}

// xterm.js loaded and the stream open: termSessions can be shown.
async function ready() {
  await loadXterm();
  connectStream();
}

/* ---------- pane ---------- */

function showPane(show) {
  if (show === !pane.hidden) return;
  pane.hidden = resizer.hidden = !show;
  footerBtn?.classList.toggle('on', show);
  store('px0.termOpen', show ? '1' : undefined);
  layout(); render();
  if (show) {
    updateNote();
    fitActive();
  }
}

export async function toggleTerminal(force) {
  if (!S.meta) return; // still booting: whether the terminal exists is not known yet
  const show = force ?? pane.hidden;
  if (show && !termMeta().available) {
    showToast('!', termMeta().reason || 'The terminal is turned off');
    return;
  }
  if (!show) {
    showPane(false);
    $('#viewport')?.focus();
    return;
  }
  showPane(true);
  if (!usable()) return;
  try {
    await ready();
    const s = termSessions.get(active);
    if (s) s.term.focus();
  } catch (e) {
    showToast('!', 'Terminal: ' + e.message);
  }
}

export const newHarnessSession = () => openSession('harness');
export const newShellSession = () => openSession('shell');

/* Alt+E with a Harness Session running: paste the selection's Reference into
   it, never pressing Enter, and hand the keyboard to the terminal so the
   instruction is typed to the harness itself. xterm's paste() uses bracketed
   paste when the program asked for it. Returns false to fall back to the
   inline composer. */
function handoff(sel) {
  const live = id => { const s = termSessions.get(id); return s && s.info.kind === 'harness' && !s.info.exited; };
  let id = live(lastHarness) ? lastHarness : null;
  if (id == null) id = [...termSessions.keys()].reverse().find(live) ?? null;
  if (id == null) return false;
  const ref = selectionRef(sel);
  showPane(true);
  selectSession(id);
  termSessions.get(id).term.paste(ref + ' ');
  showToast('✓', `Sent ${ref} to ${termSessions.get(id).info.title}`);
  return true;
}

export function initTerminal() {
  if (!pane) return;
  footerBtn?.addEventListener('click', () => toggleTerminal());
  newHarnessBtn.addEventListener('click', newHarnessSession);
  newShellBtn.addEventListener('click', newShellSession);
  $('#term-hide').addEventListener('click', () => toggleTerminal(false));
  tabsEl.addEventListener('click', e => {
    const x = e.target.closest('[data-close]');
    if (x) { closeSession(+x.dataset.close); return; }
    const tab = e.target.closest('.term-tab');
    if (tab) selectSession(+tab.dataset.id);
  });

  // Ctrl+` toggles the pane everywhere, the terminal included: it runs in the
  // capture phase, before xterm sees the key.
  addEventListener('keydown', e => {
    if (e.ctrlKey && !e.altKey && !e.metaKey && e.code === 'Backquote') {
      e.preventDefault();
      e.stopPropagation();
      toggleTerminal();
    }
  }, { capture: true });

  // Dragging the resizer sets the pane's height, remembered per browser.
  let dragging = false;
  resizer.addEventListener('mousedown', e => { dragging = true; resizer.classList.add('drag'); e.preventDefault(); });
  addEventListener('mousemove', e => {
    if (!dragging) return;
    const bottom = pane.getBoundingClientRect().bottom;
    const h = Math.max(90, Math.min(innerHeight * 0.85, bottom - e.clientY));
    pane.style.height = h + 'px';
    layout(); render();
  });
  addEventListener('mouseup', () => {
    if (!dragging) return;
    dragging = false;
    resizer.classList.remove('drag');
    store('px0.termHeight', parseInt(pane.style.height, 10));
    fitActive();
  });
  const h = parseInt(recall('px0.termHeight'), 10);
  if (h >= 90) pane.style.height = h + 'px';

  let fitTimer = 0;
  new ResizeObserver(() => { clearTimeout(fitTimer); fitTimer = setTimeout(fitActive, 40); }).observe(bodyEl);

  // Theme changes restyle every session.
  new MutationObserver(() => {
    const theme = xtermTheme();
    for (const s of termSessions.values()) s.term.options.theme = theme;
  }).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

  // Like the git stream: a page that becomes hidden stops streaming. Output
  // keeps collecting on the server and is caught up on return.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') disconnectStream();
    else if (xtermLoad) connectStream();
  });

  setHandoffHandler(handoff);
}

// The harness picked in the footer names the + Harness button.
export const refreshTerminalHarness = () => { if (pane) updateNote(); };

// Called once /api/meta is in: offer the pane, and restore it if termSessions are running.
export function applyTerminalMeta() {
  const m = termMeta();
  if (footerBtn) footerBtn.hidden = !m.available;
  updateNote();
  if (!usable()) return;
  if (m.sessions?.length) {
    ready().then(() => {
      if (recall('px0.termOpen')) showPane(true);
    }).catch(e => showToast('!', 'Terminal: ' + e.message));
  }
}
