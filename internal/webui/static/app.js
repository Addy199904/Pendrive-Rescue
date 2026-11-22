'use strict';
/* Pendrive Rescue in the browser: one column that talks in plain sentences,
   with the next step always waiting in the box at the bottom. Plain
   JavaScript, nothing from the internet. File names from the pendrive are
   only ever put on the page as text, never as HTML. */

const chat = document.getElementById('chat');
const composer = document.getElementById('composer');
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));
const nf = new Intl.NumberFormat();
const fmt = n => nf.format(n || 0);

function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const k in props || {}) {
    const v = props[k];
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'style') el.style.cssText = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (v === true) { if (k in el) el[k] = true; else el.setAttribute(k, ''); }
    else el.setAttribute(k, String(v));
  }
  add(el, kids);
  return el;
}
function add(el, kids) {
  for (const c of kids) {
    if (c == null || c === false || c === '') continue;
    if (Array.isArray(c)) add(el, c);
    else el.append(c instanceof Node ? c : String(c));
  }
}
function fill(el, ...kids) { el.replaceChildren(); add(el, kids); }

const ICONS = {
  usb: ['M9 2.5h6v5H9z', 'M10.6 4.6h.01M13.4 4.6h.01', 'M7 7.5h10v9.5a5 5 0 0 1-5 5 5 5 0 0 1-5-5z'],
  disk: ['M3 7a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z', 'M3 13.5h18', 'M17 16.3h.01'],
  file: ['M6 2.5h8l5 5v14H6z', 'M14 2.5v5h5'],
  folder: ['M3 6.5a1.5 1.5 0 0 1 1.5-1.5h4.8l2 2.2h8.2a1.5 1.5 0 0 1 1.5 1.5v9.8a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 18.5z'],
  photos: ['M4 5.5a1.5 1.5 0 0 1 1.5-1.5h13A1.5 1.5 0 0 1 20 5.5v13a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 18.5z', 'M4 16l4.5-4.5 4 4 2.5-2.5 5 5', 'M15.5 8.5h.01'],
  videos: ['M3 6.5A1.5 1.5 0 0 1 4.5 5h10A1.5 1.5 0 0 1 16 6.5v11a1.5 1.5 0 0 1-1.5 1.5h-10A1.5 1.5 0 0 1 3 17.5z', 'M16 10l5-3v10l-5-3'],
  music: ['M9 18V5.5l11-2V16', 'M9 18a3 3 0 1 1-6 0 3 3 0 0 1 6 0z', 'M20 16a3 3 0 1 1-6 0 3 3 0 0 1 6 0z'],
  documents: ['M6 2.5h8l5 5v14H6z', 'M14 2.5v5h5', 'M9 12.5h7M9 16.5h5'],
  archives: ['M4 4.5h16v4H4z', 'M5 8.5v11h14v-11', 'M10 12.5h4'],
  text: ['M5 6h14M5 10h14M5 14h9M5 18h11'],
  other: ['M6 2.5h8l5 5v14H6z', 'M14 2.5v5h5'],
  check: ['M5 12.5l4.5 4.5L19 7.5'],
  warn: ['M12 3.5l9.5 17h-19z', 'M12 10v4.5', 'M12 17.5h.01'],
  x: ['M6.5 6.5l11 11M17.5 6.5l-11 11'],
  refresh: ['M19.5 11.5A7.5 7.5 0 1 1 17.3 6', 'M19.5 4v5h-5'],
  up: ['M12 19V5.5', 'M6 11l6-6 6 6'],
  chevron: ['M9.5 6l6 6-6 6'],
};
function icon(name, cls) {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('aria-hidden', 'true');
  s.setAttribute('class', 'i' + (cls ? ' ' + cls : ''));
  for (const d of ICONS[name] || ICONS.other) {
    const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    p.setAttribute('d', d);
    s.append(p);
  }
  return s;
}

// ---------------------------------------------------------------- words and numbers

function size(b) {
  if (b == null || isNaN(b)) return '';
  if (b < 1024) return b === 1 ? '1 byte' : fmt(b) + ' bytes';
  const u = ['KB', 'MB', 'GB', 'TB'];
  let v = b / 1024, i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (v < 10 ? v.toFixed(1) : fmt(Math.round(v))) + ' ' + u[i];
}
function validDate(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d) || d.getFullYear() < 1981 || d > new Date(Date.now() + 2 * 864e5) ? null : d;
}
const month = d => d.toLocaleDateString(undefined, { year: 'numeric', month: 'long' });
function dayTime(s) {
  const d = validDate(s);
  return d ? d.toLocaleString(undefined, { year: 'numeric', month: 'long', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : 'Not known';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec || 0));
  if (sec < 50) return sec <= 1 ? 'a second' : sec + ' seconds';
  const m = Math.round(sec / 60);
  if (m < 60) return m <= 1 ? 'a minute' : m + ' minutes';
  const hh = Math.floor(m / 60), mm = m % 60;
  return (hh === 1 ? '1 hour' : hh + ' hours') + (mm ? ' ' + (mm === 1 ? '1 minute' : mm + ' minutes') : '');
}
const count = (n, one, many) => fmt(n) + ' ' + (n === 1 ? one : many);
function list(parts) {
  parts = parts.filter(Boolean);
  if (parts.length < 2) return parts.join('');
  return parts.slice(0, -1).join(', ') + ' and ' + parts[parts.length - 1];
}
const capital = s => s ? s[0].toUpperCase() + s.slice(1) : s;

const KINDS = {
  photos: ['photo', 'photos'], videos: ['video', 'videos'], music: ['music file', 'music files'],
  documents: ['document', 'documents'], archives: ['archive', 'archives'], text: ['text file', 'text files'], other: ['other file', 'other files'],
};
const OTHER = ['music', 'documents', 'archives', 'text', 'other'];

const LABELS = {
  perfect: ['Perfect', 'Checked from start to end: it is complete.'],
  partly: ['Partly damaged', ''],
  preview: ['Preview only', 'The picture itself is damaged, but the small preview the camera stored inside it is intact. It is saved next to the photo.'],
  damaged: ['Damaged', 'Part of it is missing or wrong. It is still saved, in the folder "4 - Possibly damaged files". Often it still opens partly.'],
  unknown: ['Cannot be checked', 'This kind of file cannot be tested here. It is saved anyway; open it to see.'],
  unchecked: ['Not checked yet', 'Files that were never deleted are checked when they are saved, to spare the pendrive.'],
  checking: ['Checking', 'Files are being checked one by one.'],
  wiped: ['Wiped', 'Only zeros are left where its content was. There is nothing of it to save.'],
  empty: ['Empty', 'This file holds nothing (0 bytes).'],
};
function labelOf(it) {
  const l = it.label || 'unchecked';
  if (l === 'unchecked' && checking()) return 'checking';
  return l;
}
function labelText(it) {
  const l = labelOf(it);
  if (l === 'partly') return Math.max(1, Math.round((it.share || 0) * 100)) + '% intact';
  return LABELS[l] ? LABELS[l][0] : l;
}
function labelWhy(it) {
  const l = labelOf(it);
  if (l === 'partly') {
    return 'The top ' + Math.max(1, Math.round((it.share || 0) * 100)) + '% of the picture is certainly intact. Below that it is damaged: it may turn grey or show wrong colours. The intact part is saved as its own photo next to it.';
  }
  return (LABELS[l] || ['', ''])[1];
}

const THUMBS = new Set(['jpg', 'jpeg', 'jfif', 'png', 'gif']);
const IMAGES = new Set(['jpg', 'jpeg', 'jfif', 'png', 'gif', 'bmp', 'webp', 'avif', 'ico', 'svg']);
const VIDEO = new Set(['mp4', 'm4v', 'mov', '3gp', 'webm', 'mkv', 'ogv']);
const AUDIO = new Set(['mp3', 'm4a', 'aac', 'wav', 'ogg', 'oga', 'opus', 'flac', 'amr']);
const TEXTLIKE = new Set(['docx', 'xlsx', 'pptx', 'odt', 'ods', 'odp', 'csv', 'rtf', 'vcf', 'eps', 'ps']);

// ---------------------------------------------------------------- talking to Pendrive Rescue

async function api(path, body) {
  const opt = body === undefined ? {} : {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'X-PDR': '1' }, body: JSON.stringify(body),
  };
  const r = await fetch(path, opt);
  if (r.status === 403) { gone('denied'); throw new Error('This tab is no longer connected to Pendrive Rescue.'); }
  const json = (r.headers.get('Content-Type') || '').includes('json');
  const data = json ? await r.json() : await r.text();
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  return data;
}
function say(text) { $('#live').textContent = text; }

// ---------------------------------------------------------------- state

const S = {
  hello: {}, view: '', st: null, fails: 0,
  drives: null, drivesErr: '', pick: null, base: '',
  check: null, rep: null, plan: null, mode: '',
  sum: null, tab: 'photos', show: '', items: [], total: 0, gen: 0, loading: false, sel: new Map(),
  condsAt: 0, sumAt: 0, awaitDone: false, results: null,
};
const checking = () => !!S.st && (S.st.phase !== 'ready' || S.st.checked < S.st.toCheck);

// ---------------------------------------------------------------- the conversation

function reset() {
  chat.replaceChildren();
  $('#new').hidden = true;
}
function addMsg(cls, ...kids) {
  const m = h('div', { class: 'msg ' + cls }, ...kids);
  chat.append(m);
  requestAnimationFrame(() => {
    // a long message is shown from its start, a short one just fully
    const room = innerHeight - composer.offsetHeight - 80;
    m.scrollIntoView({ block: m.offsetHeight > room ? 'start' : 'nearest', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
  });
  return m;
}
const app = (...kids) => addMsg('app', ...kids);
const user = text => addMsg('user', h('span', null, text));

// The box at the bottom holds the next step.
function setComposer(o) {
  if (!o) { fill(composer); pad(); return; }
  const btn = (b, primary) => h('button', {
    class: 'btn' + (primary ? ' primary' : ''), type: 'button', disabled: !!b.disabled, id: b.id,
    onclick: b.on,
  }, b.label);
  fill(composer, h('div', { class: 'box' },
    h('div', { class: 'say' }, o.say || ''),
    h('div', { class: 'acts' }, (o.secondary || []).map(b => btn(b, false)), o.primary ? btn(o.primary, true) : null),
    (o.alt || []).length ? h('div', { class: 'alt' }, o.alt.map(b => h('button', { class: 'link', type: 'button', onclick: b.on }, b.label))) : null));
  pad();
}
function pad() {
  requestAnimationFrame(() => chat.style.setProperty('--pad', (composer.offsetHeight + 48) + 'px'));
}
addEventListener('resize', pad);

// ---------------------------------------------------------------- dialogs

function dialog(title, body, buttons, cls) {
  return new Promise(resolve => {
    const id = 'd' + Math.random().toString(36).slice(2);
    const dlg = h('dialog', { class: cls || '', 'aria-labelledby': id });
    const close = v => { dlg.close(); dlg.remove(); resolve(v); };
    dlg.append(h('div', { class: 'dlg' },
      h('header', null, h('h2', { id }, title)),
      h('div', { class: 'body' }, body),
      h('footer', null, buttons.map(([label, value, primary]) =>
        h('button', { class: 'btn' + (primary ? ' primary' : ''), type: 'button', onclick: () => close(value) }, label)))));
    dlg.addEventListener('cancel', e => { e.preventDefault(); close(undefined); });
    document.body.append(dlg);
    dlg.showModal();
  });
}
const confirmBox = (title, text, ok) => dialog(title, h('p', null, text), [['Cancel', false], [ok, true, true]]);
const messageBox = (title, text) => dialog(title, h('p', null, text), [['OK', true, true]]);

function note(kind, title, text) {
  return h('div', { class: 'note' + (kind === 'bad' ? ' bad' : ''), role: kind === 'bad' ? 'alert' : null },
    icon('warn'), h('div', null, h('b', null, title), text ? h('span', null, text) : null));
}

// A folder (or safe copy) chooser that works on this computer's drives.
function openPicker(o, onChoose) {
  let cur = null;
  const roots = h('div', { class: 'roots' });
  const where = h('div', { class: 'pk-path' });
  const ul = h('ul', { class: 'pk-list' });
  const choose = o.files ? null : h('button', { class: 'btn primary', type: 'button', onclick: () => { close(); onChoose(cur.path); } }, o.pick || 'Save here');
  const dlg = h('dialog', { class: 'picker', 'aria-labelledby': 'pk-t' },
    h('div', { class: 'dlg' },
      h('header', null, h('h2', { id: 'pk-t' }, o.title),
        h('p', { class: 'hint' }, o.hint || (o.files ? 'Choose the .img file of a safe copy that Pendrive Rescue made earlier.' :
          'A new folder for the files is made inside the place you choose. Choose a folder that holds a safe copy to continue an earlier recovery.'))),
      h('div', { class: 'body' }, roots, where, ul),
      h('footer', null, h('button', { class: 'btn', type: 'button', onclick: () => close() }, 'Cancel'), choose)));
  const close = () => { dlg.close(); dlg.remove(); };
  dlg.addEventListener('cancel', e => { e.preventDefault(); close(); });
  async function load(path) {
    if (choose) choose.disabled = true;
    fill(ul, h('li', { class: 'empty' }, 'Opening…'));
    let r;
    try { r = await api('/api/folders?' + new URLSearchParams({ path: path || '', files: o.files ? '1' : '' })); }
    catch (e) { fill(ul, h('li', { class: 'empty' }, e.message)); return; }
    cur = r;
    const sep = r.path.includes('\\') ? '\\' : '/';
    const under = x => r.path === x.path || r.path.startsWith(x.path.endsWith(sep) ? x.path : x.path + sep);
    const best = (r.roots || []).filter(under).sort((a, b) => b.path.length - a.path.length)[0];
    fill(roots, ...(r.roots || []).map(x => h('button', { class: x === best ? 'on' : '', type: 'button', onclick: () => load(x.path) },
      icon(x.path.length <= 3 ? 'disk' : 'folder'), x.name, x.free ? h('span', { class: 'c' }, size(x.free) + ' free') : null)));
    fill(where, r.parent ? h('button', { class: 'quiet', type: 'button', onclick: () => load(r.parent) }, icon('up'), 'Up') : null,
      h('span', { class: 'here' }, r.path), r.free ? h('span', { class: 'hint' }, size(r.free) + ' free') : null);
    const rows = [];
    for (const d of r.dirs) {
      rows.push(h('li', null, h('button', { class: 'pk-item', type: 'button', onclick: () => load(d.path) },
        icon('folder'), h('span', null, d.name), d.rescue ? h('span', { class: 'tag' }, 'has a safe copy') : null)));
    }
    for (const f of r.files) {
      rows.push(h('li', null, h('button', { class: 'pk-item', type: 'button', onclick: () => { close(); onChoose(f); } },
        icon('file'), h('span', null, f.name), h('span', { class: 'hint' }, size(f.size)))));
    }
    if (!rows.length) rows.push(h('li', { class: 'empty' }, r.error || (o.files ? 'No .img files in this folder.' : 'No folders in here. You can choose this one.')));
    fill(ul, ...rows);
    if (choose) choose.disabled = !!r.error;
  }
  document.body.append(dlg);
  dlg.showModal();
  load(o.start || '');
}

// ---------------------------------------------------------------- 1. choose the drive

const driveName = d => d.model || (d.usb ? 'USB drive' : d.number >= 0 ? 'Disk ' + d.number : d.path);

function showStart() {
  S.view = 'start';
  stopPolls();
  reset();
  Object.assign(S, { pick: null, check: null, rep: null, plan: null, base: '', st: null, sum: null, items: [], sel: new Map(), results: null });
  const hl = S.hello || {};
  const notes = [];
  if (hl.temp) notes.push(note('bad', 'Pendrive Rescue is running from inside the ZIP file',
    'Close it, right-click the ZIP file, choose "Extract All", then start Pendrive Rescue from the extracted folder.'));
  if (hl.os && !hl.admin) notes.push(note('bad', 'Pendrive Rescue cannot read drives directly',
    hl.os === 'windows' ? 'Close it, right-click it and choose "Run as administrator". You can still open a safe copy.' : 'Start it with sudo to read drives directly. You can still open a safe copy.'));
  if (hl.os === 'windows') notes.push(note('warn', 'If Windows asks you to format the pendrive, click Cancel', 'Formatting writes over lost files. Pendrive Rescue never needs it.'));
  app(h('p', { class: 'lead' }, 'Plug in the pendrive or memory card that is giving you trouble, then pick it below.'),
    h('p', null, 'I check it first and tell you what is wrong. I only read it, so nothing on it can get worse.'),
    notes,
    h('div', { class: 'card' },
      h('ul', { class: 'drives', id: 'drives', role: 'radiogroup', 'aria-label': 'Drives on this computer' }, h('li', { class: 'empty' }, 'Looking for drives…')),
      h('div', { class: 'card-foot' },
        h('button', { class: 'quiet', type: 'button', onclick: loadDrives }, icon('refresh'), 'Refresh the list'),
        h('button', { class: 'quiet', type: 'button', onclick: openImage }, icon('file'), 'Open a safe copy'))));
  startComposer();
  loadDrives();
}

function startComposer() {
  const p = S.pick;
  setComposer({
    say: p ? [h('b', null, p.name), ', ' + size(p.size)] : 'Pick a drive to check it.',
    primary: { label: p && !p.device ? 'Look inside this copy' : 'Check this pendrive', disabled: !p, on: check, id: 'go' },
    alt: [{ label: 'Repair photos I already copied', on: chooseRepair }],
  });
}

async function loadDrives() {
  try {
    const r = await api('/api/drives');
    S.drives = r.drives || [];
    S.drivesErr = r.error || '';
  } catch (e) {
    S.drives = [];
    S.drivesErr = e.message;
  }
  renderDrives();
}

function renderDrives() {
  const ul = $('#drives');
  if (!ul) return;
  const rank = d => (d.usb || d.removable ? 0 : d.system ? 2 : 1);
  const rows = [...(S.drives || [])].sort((a, b) => rank(a) - rank(b)).map(d => {
    const on = S.pick && S.pick.path === d.path;
    const dead = !(d.size > 0);
    const meta = [];
    if (d.letters && d.letters.length) meta.push(d.letters.join(' '));
    if (d.fs && d.fs.length) meta.push(d.fs.join(', '));
    meta.push(d.usb ? 'USB' : d.removable ? 'Removable' : 'Built in');
    if (d.note) meta.push(d.note);
    return h('li', null, h('label', { class: 'drive' + (on ? ' on' : '') + (d.system ? ' sys' : '') + (dead ? ' dead' : '') },
      h('input', { type: 'radio', name: 'drive', value: d.path, checked: on, disabled: dead, onchange: () => pickDrive(d) }),
      h('span', { class: 'ic' }, icon(d.usb || d.removable ? 'usb' : 'disk', 'big')),
      h('span', null, h('span', { class: 'name' }, driveName(d)),
        h('span', { class: 'meta' }, dead ? 'Shows no size. Its controller chip has probably failed: no program can read it, but a data recovery lab can. Try another USB port first.' : meta.join(', '))),
      h('span', { class: 'size' }, dead ? 'No size' : size(d.size))));
  });
  if (S.pick && !S.pick.device) {
    rows.push(h('li', null, h('label', { class: 'drive on' },
      h('input', { type: 'radio', name: 'drive', checked: true }),
      h('span', { class: 'ic' }, icon('file', 'big')),
      h('span', null, h('span', { class: 'name' }, S.pick.name), h('span', { class: 'meta' }, 'Safe copy, ' + S.pick.path)),
      h('span', { class: 'size' }, size(S.pick.size)))));
  }
  if (!rows.length) {
    rows.push(h('li', { class: 'empty' }, S.drivesErr ? 'The drives could not be listed: ' + S.drivesErr :
      'No drives found. Plug in the pendrive, wait a few seconds, then press "Refresh the list".'));
  }
  fill(ul, ...rows);
}

async function pickDrive(d) {
  if (d.system && !(await confirmBox('Your computer runs from this disk',
    'Files lost from a pendrive are not on this disk. Check it anyway? Reading it can take a long time.', 'Check it anyway'))) {
    renderDrives();
    return;
  }
  S.pick = { path: d.path, name: driveName(d), desc: driveName(d) + ' (' + size(d.size) + ')', size: d.size, device: true };
  renderDrives();
  startComposer();
}

function openImage() {
  openPicker({ files: true, title: 'Open a safe copy', start: S.base }, f => {
    S.pick = { path: f.path, name: f.name, desc: f.name + ' (safe copy, ' + size(f.size) + ')', size: f.size, device: false };
    renderDrives();
    startComposer();
  });
}

// ---------------------------------------------------------------- 2. check it

let checkTimer = 0;
async function check() {
  const p = S.pick;
  if (!p) return;
  if (!p.device) { // a safe copy: nothing to check
    user('Look inside ' + p.name);
    $('#new').hidden = false;
    S.rep = null;
    return plan();
  }
  user('Check ' + p.name);
  $('#new').hidden = false;
  const card = h('div', { class: 'card prog' },
    h('div', { class: 'line' }, h('span', { class: 'working', id: 'ck-step' }, 'Starting'), h('span', { class: 'num', id: 'ck-num' })),
    h('div', { class: 'meter busy', id: 'ck-meter' }, h('i')));
  S.view = 'check';
  app(h('p', null, 'I am checking it. This takes about a minute, and I only read.'), card);
  setComposer({
    say: 'Checking ' + p.name,
    secondary: [{ label: 'Stop', on: async () => { await api('/api/check', { action: 'stop' }).catch(() => {}); } }],
  });
  try {
    await api('/api/check', { source: p.path, desc: p.desc });
  } catch (e) {
    app(h('p', null, capital(e.message)));
    startComposer();
    return;
  }
  pollCheck();
}

async function pollCheck() {
  clearTimeout(checkTimer);
  if (S.view !== 'check') return;
  let st;
  try { st = await api('/api/check'); } catch (e) { checkTimer = setTimeout(pollCheck, 1000); return; }
  if (st.running) {
    $('#ck-step').textContent = st.step || 'Checking';
    const m = $('#ck-meter');
    if (st.total > 1) {
      m.classList.remove('busy');
      m.firstChild.style.width = Math.round(100 * st.done / st.total) + '%';
      $('#ck-num').textContent = Math.round(100 * st.done / st.total) + '%';
    } else {
      m.classList.add('busy');
      $('#ck-num').textContent = '';
    }
    checkTimer = setTimeout(pollCheck, 400);
    return;
  }
  const card = $('#ck-step') && $('#ck-step').closest('.card');
  if (card) card.remove();
  S.rep = st.report;
  verdict(st.report);
}

const LEVEL = { healthy: 'good', slow: 'good', worn: 'warn', fake: 'bad', failing: 'bad', gone: 'bad', dead: 'bad' };

function verdict(rep) {
  S.view = 'verdict';
  if (!rep) return;
  const ms = [];
  ms.push([fmt(rep.spots + rep.unreadable), 'places read three times']);
  if (rep.unstableAt) ms.push([fmt(rep.unstableAt), 'read differently each time']);
  if (rep.unreadable) ms.push([fmt(rep.unreadable), 'could not be read']);
  if (rep.slow) ms.push([fmt(rep.slow), 'slower than a second']);
  if (rep.speed > 0) ms.push([size(rep.speed) + '/s', 'reading speed']);
  if (rep.realSize > 0) ms.push([size(rep.realSize), 'it really holds']);
  app(h('div', { class: 'verdict ' + (LEVEL[rep.verdict] || 'good') },
    h('div', { class: 'kicker' }, h('span', { class: 'dot' }), 'Checked in ' + dur(rep.seconds)),
    h('h1', null, rep.title),
    h('p', null, rep.detail)),
    rep.spots + rep.unreadable > 0 ? h('div', { class: 'card measures' }, ms.map(([v, k]) => h('div', null, h('b', null, v), h('span', null, k)))) : null,
    h('p', { style: 'margin-top:16px' }, rep.next),
    h('p', { class: 'hint' }, h('button', { class: 'link', type: 'button', onclick: () => window.open('/api/shop', '_blank', 'noopener') }, 'A note to take to a repair shop'),
      rep.lab ? ' explains when only a lab can help.' : ' says what to expect and what to ask.'));
  if (rep.verdict === 'gone' || rep.verdict === 'dead') {
    setComposer({
      say: 'Plug it into another USB port first.',
      primary: { label: 'Check it again', on: check },
      secondary: [{ label: 'Choose another drive', on: showStart }],
    });
    return;
  }
  plan();
}

// ---------------------------------------------------------------- 3. plan and start

async function plan() {
  const p = S.pick, rep = S.rep;
  let pl;
  try { pl = await api('/api/plan', { source: p.path, base: S.base, need: rep && rep.realSize || 0 }); }
  catch (e) { pl = { error: e.message }; }
  S.plan = pl;
  const want = rep ? rep.plan : 'direct';
  let mode = want;
  let why = '';
  if (!pl.device) mode = 'image';
  else if (pl.resume > 0) mode = want === 'careful' ? 'careful' : 'copy';
  else if (want !== 'direct' && !pl.canImage) { mode = 'direct'; why = pl.imageWhy; }
  S.mode = mode;
  if (why) {
    app(h('p', null, 'There is not enough room for a copy in that folder. ' + why +
      ' I can read the pendrive directly instead, and read damaged photos again as I go. Or choose a folder on a bigger drive.'));
  }
  if (!pl.device) app(h('p', null, 'This is a safe copy made earlier, so the pendrive itself is not needed. I will look for files in it.'));
  planComposer();
}

const MODE = {
  careful: 'Copy it carefully and find my files',
  copy: 'Copy it and find my files',
  direct: 'Find my files',
  image: 'Find my files',
};

function planComposer() {
  const pl = S.plan || {};
  let label = MODE[S.mode];
  if (pl.resume > 0 && S.mode !== 'direct') label = 'Continue the copy (' + size(pl.resume) + ' done)';
  const alt = [{ label: 'Choose another folder', on: chooseFolder }];
  if (pl.device && S.mode !== 'direct') alt.push({ label: 'Read it directly instead', on: () => { S.mode = 'direct'; planComposer(); } });
  if (pl.device && S.mode === 'direct' && pl.canImage) alt.push({ label: 'Make a copy first instead', on: () => { S.mode = S.rep && S.rep.plan === 'careful' ? 'careful' : 'copy'; planComposer(); } });
  setComposer({
    say: pl.error ? h('span', { style: 'color:var(--bad)' }, pl.error)
      : [h('span', { class: 'where' }, 'Files go to ', h('b', { title: pl.outDir }, pl.outDir)),
        pl.free ? h('span', null, size(pl.free) + ' free there' + (S.mode === 'careful' || S.mode === 'copy' ? ', the copy needs up to ' + size((S.rep && S.rep.realSize) || pl.size) : '')) : null],
    primary: { label, disabled: !!pl.error || !pl.outDir, on: startRecovery, id: 'go' },
    alt,
  });
}

function chooseFolder() {
  openPicker({ files: false, title: 'Choose where to save the files', start: S.plan && S.plan.base }, path => {
    S.base = path;
    plan();
  });
}

async function startRecovery() {
  const p = S.pick, pl = S.plan, mode = S.mode;
  const go = $('#go');
  const label = go ? go.textContent : MODE[mode];
  if (go) { go.disabled = true; go.textContent = 'Starting…'; }
  const copy = mode === 'careful' || mode === 'copy';
  try {
    S.st = await api('/api/start', {
      source: p.path, desc: p.desc, outDir: pl.outDir, image: copy, resume: copy && pl.resume > 0,
      deep: true, text: true, careful: mode === 'careful', limit: (S.rep && S.rep.realSize) || 0,
    });
  } catch (e) {
    if (go) { go.disabled = false; go.textContent = label; }
    messageBox('Reading could not start', e.message);
    return;
  }
  user(label);
  reading();
  poll();
}

// ---------------------------------------------------------------- 4. read it

const TITLES = {
  quick: 'Taking a quick look', reading: 'Reading the pendrive', retrying: 'Trying the unreadable parts again',
  analysing: 'Looking for lost files', ready: 'Finished reading',
};

function reading() {
  S.view = 'reading';
  $('#new').hidden = false;
  const cells = [];
  for (let i = 0; i < 100; i++) cells.push(h('i'));
  app(h('p', null, S.st.careful ? 'Copying it carefully. Every part is read twice, and wherever the reads disagree I read it again and keep what most reads agree on.'
      : S.st.imagePath || S.mode === 'copy' ? 'Copying it to this computer first, then looking for your files in the copy.'
        : 'Reading it once from start to end and looking for your files.'),
    h('div', { class: 'card prog', id: 'rd' },
      h('div', { class: 'line' }, h('span', { class: 'working', id: 'rd-title' }, 'Starting'), h('span', { class: 'num', id: 'rd-pct' })),
      h('div', { class: 'meter', id: 'rd-meter' }, h('i')),
      h('div', { class: 'hint', id: 'rd-line' }),
      h('div', { class: 'strip', id: 'rd-strip', role: 'img', 'aria-label': 'Map of the pendrive: each block is one hundredth of it' }, cells),
      h('div', { class: 'strip-key' },
        h('span', { style: '--k:color-mix(in srgb,var(--ink) 62%,var(--surface))' }, 'data'),
        h('span', { style: '--k:var(--sand-2)' }, 'empty'),
        h('span', { style: '--k:var(--bad)' }, 'unreadable'),
        h('span', { style: '--k:var(--sand)' }, 'not read yet')),
      h('ul', { class: 'facts', id: 'rd-facts' }),
      h('div', { id: 'rd-alert' })));
  setComposer({
    say: 'Reading ' + (S.pick ? S.pick.name : S.st.drive),
    secondary: [{ label: 'Stop reading', on: stopReading, id: 'stop' }],
  });
  updateReading(S.st);
}

let alertKey = '';
function updateReading(st) {
  const box = $('#rd');
  if (!box) return;
  const pct = st.total > 0 ? Math.min(1, st.done / st.total) : 0;
  $('#rd-title').textContent = st.phase === 'reading' ? (st.step || TITLES.reading) : (TITLES[st.phase] || st.step);
  const m = $('#rd-meter');
  const busy = (st.phase === 'quick' || st.phase === 'analysing') && !(st.total > 0);
  m.classList.toggle('busy', busy);
  m.firstChild.style.width = (st.phase === 'ready' ? 100 : pct * 100) + '%';
  $('#rd-pct').textContent = busy ? '' : Math.floor((st.phase === 'ready' ? 1 : pct) * 100) + '%';
  let line = '';
  if (st.phase === 'reading') {
    line = size(st.done) + ' of ' + size(st.total);
    if (st.left > 0) line += ', about ' + dur(st.left) + ' left';
    if (st.speed > 0) line += ', ' + size(st.speed) + ' per second';
  } else if (st.phase === 'retrying') {
    line = size(st.total) + ' of unreadable parts';
  } else if (st.phase === 'ready') {
    line = 'Took ' + dur(st.elapsed);
  } else {
    line = 'Time so far: ' + dur(st.elapsed);
  }
  $('#rd-line').textContent = line;
  const cells = $('#rd-strip').children;
  const head = st.phase === 'reading' ? Math.min(99, Math.floor(pct * 100)) : -1;
  for (let i = 0; i < cells.length; i++) {
    const c = i === head ? 'now' : 's' + ((st.map && st.map[i]) || 0);
    if (cells[i].className !== c) cells[i].className = c;
  }
  const found = Object.keys(KINDS).map(g => st.counts[g] ? count(st.counts[g], KINDS[g][0], KINDS[g][1]) : '').filter(Boolean);
  const facts = [];
  facts.push(h('li', null, 'Found so far: ', found.length ? h('b', null, list(found)) : 'nothing yet'));
  if (st.settled) facts.push(h('li', null, h('b', null, fmt(st.settled)), ' small parts read differently and were settled by reading them again'));
  if (st.badBytes) facts.push(h('li', null, h('b', null, size(st.badBytes)), ' could not be read so far'));
  fill($('#rd-facts'), ...facts);
  let key = '', al = null;
  if (st.stall >= 15 && st.phase !== 'ready') {
    key = 'stall' + Math.floor(st.stall / 30);
    al = note('bad', 'The pendrive has not answered for ' + dur(st.stall),
      'Damaged parts can take minutes, so waiting is often worth it. If nothing changes for 10 minutes, press "Stop reading", plug the pendrive into another USB port and start again' +
      (st.imagePath || S.mode !== 'direct' ? ', choosing the same folder: the copy continues where it stopped.' : '.'));
  }
  if (key !== alertKey) { fill($('#rd-alert'), al || ''); alertKey = key; }
  const stop = $('#stop');
  if (stop) { stop.disabled = !!st.stopped || st.phase === 'analysing'; if (st.stopped) stop.textContent = 'Stopping…'; }
}

async function stopReading() {
  if (!(await confirmBox('Stop reading the pendrive?',
    'The files found so far stay available. Parts not read yet are not searched. With a copy you can continue later by choosing the same folder.',
    'Stop reading'))) return;
  try { await api('/api/stop', {}); } catch (e) { messageBox('Could not stop', e.message); }
}

// ---------------------------------------------------------------- polling

let pollTimer = 0, inFlight = false;
function stopPolls() { clearTimeout(pollTimer); clearTimeout(checkTimer); clearTimeout(repairTimer); }
async function poll() {
  clearTimeout(pollTimer);
  if (inFlight) return;
  inFlight = true;
  let st;
  try {
    st = await api('/api/status');
    S.fails = 0;
  } catch (e) {
    inFlight = false;
    if (S.view === 'gone') return;
    if (++S.fails >= 4) { gone('closed'); return; }
    pollTimer = setTimeout(poll, 1500);
    return;
  }
  inFlight = false;
  if (st.phase === 'none') return;
  const prev = S.st;
  S.st = st;
  try { onStatus(st, prev); } catch (e) { console.error(e); }
  const busy = st.phase !== 'ready' || st.save.running || st.checked < st.toCheck || S.awaitDone;
  if (S.view !== 'start' && S.view !== 'gone' && S.view !== 'repair') pollTimer = setTimeout(poll, busy ? 700 : 2500);
}

function onStatus(st, prev) {
  if (st.phase === 'failed') { if (S.view !== 'failed') failed(st); return; }
  if (S.view === 'reading') {
    if (st.phase === 'ready') { updateReading(st); const card = $('#rd'); if (card) card.querySelector('.working').classList.remove('working'); results(); return; }
    updateReading(st);
  } else if (S.view === 'results') {
    updateResults(st, prev);
  }
}

// ---------------------------------------------------------------- 5. what was found

async function results() {
  S.view = 'results';
  $('#new').hidden = false;
  try { S.sum = await api('/api/summary'); } catch (e) { S.sum = { groups: {} }; }
  const g = S.sum.groups || {};
  const n = k => (g[k] && g[k].files) || 0;
  const other = OTHER.reduce((a, k) => a + n(k), 0);
  S.tab = n('photos') ? 'photos' : n('videos') ? 'videos' : 'other';
  S.results = app(h('div', { id: 'res-words' }));
  renderWords();
  if (!S.sum.files) {
    resultsComposer();
    return;
  }
  addMsg('wide', h('div', { class: 'card', id: 'res' },
    h('div', { class: 'tabs', role: 'tablist', id: 'res-tabs' }),
    h('div', { id: 'res-body' })));
  renderTabs();
  loadItems(true);
  resultsComposer();
}

// the sentences above the gallery: what was found, and in what state
function renderWords() {
  const el = $('#res-words');
  if (!el || !S.sum) return;
  const g = S.sum.groups || {};
  const n = k => (g[k] && g[k].files) || 0;
  const lab = (k, l) => (g[k] && g[k].labels && g[k].labels[l]) || 0;
  const other = OTHER.reduce((a, k) => a + n(k), 0);
  const words = [];
  if (!S.sum.files) {
    const d = (S.st && S.st.diagnosis || []).find(x => x.level === 'bad' || x.level === 'warn');
    words.push(h('p', { class: 'lead' }, 'I did not find any files.'), d ? h('p', null, d.title + '. ' + d.text) : null);
    fill(el, ...words);
    return;
  }
  words.push(h('p', { class: 'lead' }, 'I found ' + list([n('photos') ? count(n('photos'), 'photo', 'photos') : '',
    n('videos') ? count(n('videos'), 'video', 'videos') : '', other ? count(other, 'other file', 'other files') : '']) + '.'));
  if (n('photos')) {
    const parts = [];
    if (lab('photos', 'perfect')) parts.push(count(lab('photos', 'perfect'), 'is perfect', 'are perfect'));
    if (lab('photos', 'partly')) parts.push(fmt(lab('photos', 'partly')) + ' partly damaged');
    if (lab('photos', 'preview')) parts.push(fmt(lab('photos', 'preview')) + ' with only their small preview left');
    if (lab('photos', 'damaged')) parts.push(fmt(lab('photos', 'damaged')) + ' damaged beyond that');
    let s = parts.length ? 'Of the photos, ' + list(parts) + '.' : '';
    if (checking() && S.st.toCheck) s += ' I am still checking the rest (' + fmt(S.st.checked) + ' of ' + fmt(S.st.toCheck) + ' done).';
    else if (lab('photos', 'unchecked')) s += ' ' + capital(count(lab('photos', 'unchecked'), 'photo was', 'photos were')) + ' never deleted; those are checked when you save them.';
    if (s) words.push(h('p', null, s.trim()));
  }
  const first = validDate(S.sum.first), last = validDate(S.sum.last);
  if (first && last && n('photos') + n('videos') > 1) {
    words.push(h('p', { class: 'hint' }, first.getFullYear() === last.getFullYear() && first.getMonth() === last.getMonth()
      ? 'They are from ' + month(first) + '.' : 'They are from ' + month(first) + ' to ' + month(last) + '.'));
  }
  fill(el, ...words);
}

function renderTabs() {
  const el = $('#res-tabs');
  if (!el || !S.sum) return;
  const g = S.sum.groups || {};
  const n = k => (g[k] && g[k].files) || 0;
  const tabs = [['photos', 'Photos', n('photos')], ['videos', 'Videos', n('videos')], ['other', 'Other files', OTHER.reduce((a, k) => a + n(k), 0)]];
  const sel = h('select', { 'aria-label': 'Show', onchange: e => { S.show = e.target.value; loadItems(true); } },
    [['', 'Everything'], ['perfect', 'Only perfect ones'], ['partly,preview,damaged', 'Only damaged ones'], ['unknown,unchecked', 'Not checked']].map(([v, t]) =>
      h('option', { value: v, selected: S.show === v }, t)));
  fill(el, ...tabs.filter(t => t[2] > 0 || t[0] === S.tab).map(([k, label, c]) =>
    h('button', { class: 'tab', role: 'tab', type: 'button', 'aria-selected': String(S.tab === k), onclick: () => { S.tab = k; renderTabs(); loadItems(true); } },
      label, h('span', { class: 'c' }, fmt(c)))),
  h('span', { class: 'grow' }), sel);
}

function query(withPage) {
  const q = { groups: S.tab === 'other' ? OTHER.join(',') : S.tab, label: S.show, sort: S.tab === 'other' ? 'type' : 'date', desc: S.tab === 'other' ? '' : '1' };
  if (withPage) { q.offset = String(S.items.length); q.limit = '200'; }
  return new URLSearchParams(q);
}

let io = null;
async function loadItems(resetList) {
  if (!resetList && S.loading) return;
  if (resetList) { S.gen++; S.items = []; }
  const gen = S.gen;
  S.loading = true;
  let page;
  try { page = await api('/api/items?' + query(true)); } catch (e) { S.loading = false; return; }
  if (gen !== S.gen) return;
  S.loading = false;
  const start = S.items.length;
  S.items.push(...(page.items || []));
  S.total = page.total;
  renderItems(resetList, start);
}

function renderItems(resetList, start) {
  const body = $('#res-body');
  if (!body) return;
  if (resetList) {
    fill(body, S.tab === 'other' ? h('ul', { class: 'rows', id: 'res-rows' }) : h('div', { class: 'days', id: 'res-days' }), h('div', { class: 'more', id: 'res-more' }));
    if (io) io.disconnect();
    io = new IntersectionObserver(es => { if (es.some(e => e.isIntersecting) && S.items.length < S.total) loadItems(false); }, { rootMargin: '800px' });
    io.observe($('#res-more'));
  }
  if (S.tab === 'other') {
    const ul = $('#res-rows');
    for (let i = start; i < S.items.length; i++) ul.append(row(S.items[i], i));
  } else {
    const days = $('#res-days');
    for (let i = start; i < S.items.length; i++) {
      const it = S.items[i];
      const d = validDate(it.mtime);
      const key = d ? d.getFullYear() + '-' + d.getMonth() : 'none';
      let grid = days.lastElementChild && days.lastElementChild.dataset.key === key ? $('.grid', days.lastElementChild) : null;
      if (!grid) {
        grid = h('div', { class: 'grid' });
        days.append(h('section', { class: 'day', 'data-key': key }, h('h3', null, d ? month(d) : 'Date not known'), grid));
      }
      grid.append(tile(it, i));
    }
  }
  const more = $('#res-more');
  more.textContent = S.items.length < S.total ? 'Loading more…' : S.total ? '' : S.show ? 'Nothing here with this choice.' : 'Nothing here.';
  if (S.items.length < S.total) { io.unobserve(more); io.observe(more); }
}

function tile(it, i) {
  const l = labelOf(it);
  const pic = THUMBS.has(it.ext) && l !== 'wiped'
    ? h('img', { src: '/api/thumb?id=' + it.id, alt: '', loading: 'lazy', decoding: 'async', onerror: e => e.target.replaceWith(ph(it)) })
    : ph(it);
  const t = h('button', {
    class: 'tile' + (S.sel.has(it.id) ? ' on' : ''), type: 'button', 'data-id': it.id,
    'aria-label': it.name + ', ' + labelText(it), title: it.name,
    onclick: e => { if (e.target.closest('.sel')) { toggle(it); return; } view(i); },
    onkeydown: e => { if (e.key === ' ' || e.key === 'x') { e.preventDefault(); toggle(it); } },
  }, pic,
  h('span', { class: 'sel', title: 'Select', role: 'checkbox', 'aria-checked': String(S.sel.has(it.id)) }, icon('check')),
  l === 'perfect' ? h('span', { class: 'label perfect', title: 'Perfect' }, h('span', null, icon('check')))
    : l === 'unchecked' || l === 'empty' ? null
      : h('span', { class: 'label ' + l }, h('span', null, labelText(it))));
  return t;
}
function ph(it) {
  return h('span', { class: 'ph' }, h('span', null, icon(it.group), (it.ext || '').toUpperCase()));
}
function row(it, i) {
  const l = labelOf(it);
  const where = (it.dir || []).slice(1).join(' / ');
  return h('li', { class: S.sel.has(it.id) ? 'on' : '', 'data-id': it.id, onclick: e => { if (e.target.tagName !== 'INPUT') view(i); } },
    h('input', { type: 'checkbox', checked: S.sel.has(it.id), 'aria-label': 'Select ' + it.name, onchange: () => toggle(it) }),
    icon(it.group),
    h('span', { class: 'nm' }, h('b', null, it.name), h('span', null, where || ' ')),
    h('span', { class: 'sz' }, size(it.size)),
    h('span', { class: 'chip ' + l }, labelText(it)));
}

function toggle(it) {
  if (S.sel.has(it.id)) S.sel.delete(it.id); else S.sel.set(it.id, it.size);
  const on = S.sel.has(it.id);
  $$('[data-id="' + it.id + '"]').forEach(el => {
    el.classList.toggle('on', on);
    const c = $('input[type=checkbox]', el);
    if (c) c.checked = on;
    const s = $('.sel', el);
    if (s) s.setAttribute('aria-checked', String(on));
  });
  resultsComposer();
}

function resultsComposer() {
  const st = S.st || {};
  if (st.save && st.save.running) return;
  const n = S.sel.size;
  let bytes = 0;
  for (const v of S.sel.values()) bytes += v;
  const files = S.sum ? S.sum.files : st.files;
  const alt = [];
  if (n) alt.push({ label: 'Clear the selection', on: () => { S.sel.clear(); $$('.tile.on, .rows li.on').forEach(el => el.classList.remove('on')); $$('.rows input').forEach(c => { c.checked = false; }); resultsComposer(); } });
  alt.push({ label: 'What happened to the pendrive', on: diagnosis });
  alt.push({ label: 'A note for a repair shop', on: () => window.open('/api/shop', '_blank', 'noopener') });
  setComposer({
    say: n ? [h('b', null, count(n, 'file', 'files') + ' selected'), ', ' + size(bytes)]
      : h('span', { class: 'where' }, 'Files go to ', h('b', { title: st.outDir }, st.outDir)),
    secondary: n ? [{ label: 'Save everything', on: () => save({ all: true }, 'Save everything') }] : [],
    primary: n ? { label: 'Save ' + count(n, 'file', 'files'), on: () => save({ ids: [...S.sel.keys()] }, 'Save the ' + count(n, 'file', 'files') + ' I selected') }
      : { label: files === 1 ? 'Save the file' : 'Save all ' + fmt(files) + ' files', disabled: !files, on: () => save({ all: true }, 'Save everything') },
    alt,
  });
}

function updateResults(st, prev) {
  if (st.save.running || S.awaitDone) { updateSaving(st); return; }
  const now = Date.now();
  const wasChecking = prev && (prev.phase !== 'ready' || prev.checked < prev.toCheck);
  if ((checking() && now - S.condsAt > 2500) || (wasChecking && !checking())) refreshConds();
  if ((checking() && now - S.sumAt > 3000) || (wasChecking && !checking())) refreshSummary();
}

async function refreshSummary() {
  S.sumAt = Date.now();
  try { S.sum = await api('/api/summary'); } catch (e) { return; }
  renderWords();
  renderTabs();
}

async function refreshConds() {
  S.condsAt = Date.now();
  const ids = S.items.slice(0, 3000).map(x => x.id);
  if (!ids.length) return;
  let r;
  try { r = await api('/api/conds', { ids }); } catch (e) { return; }
  S.items.forEach((it, i) => {
    const c = r[it.id];
    if (!c) return;
    const [label, share] = c[3].split('|');
    const was = labelOf(it);
    it.cond = c[0]; it.note = c[1]; it.saved = c[2]; it.label = label; it.share = +share;
    if (labelOf(it) === was && !(was === 'checking' && !checking())) return;
    const el = $('#res-body [data-id="' + it.id + '"]');
    if (el) el.replaceWith(S.tab === 'other' ? row(it, i) : tile(it, i));
  });
}

function diagnosis() {
  const st = S.st || {};
  const items = (st.diagnosis || []).map(d => h('li', { class: d.level }, h('span', { class: 'ic' }), h('div', null, h('b', null, d.title), d.text ? h('p', null, d.text) : null)));
  const notes = (st.notes || []).map(n => h('li', { class: 'info' }, h('span', { class: 'ic' }), h('div', null, h('p', null, n))));
  dialog('What happened to the pendrive', items.length || notes.length ? h('ul', { class: 'diag' }, items, notes) : h('p', null, 'Nothing unusual was found.'), [['Close', true, true]]);
}

// ---------------------------------------------------------------- one file, large

function view(i) {
  let cur = i;
  const stage = h('div', { class: 'stage' });
  const side = h('div', { class: 'side' });
  const dlg = h('dialog', { class: 'viewer', 'aria-label': 'File' },
    h('div', { class: 'wrap' }, stage, side),
    h('button', { class: 'quiet x', type: 'button', onclick: () => close(), 'aria-label': 'Close' }, icon('x')));
  const close = () => { dlg.close(); dlg.remove(); };
  dlg.addEventListener('cancel', e => { e.preventDefault(); close(); });
  dlg.addEventListener('keydown', e => {
    if (e.target.tagName === 'VIDEO' || e.target.tagName === 'AUDIO') return;
    if (e.key === 'ArrowRight') { e.preventDefault(); go(1); }
    if (e.key === 'ArrowLeft') { e.preventDefault(); go(-1); }
  });
  const go = d => {
    const n = cur + d;
    if (n < 0) return;
    if (n >= S.items.length) { if (S.items.length < S.total) loadItems(false); return; }
    cur = n;
    show();
  };
  let gen = 0;
  function show() {
    const it = S.items[cur];
    const g = ++gen;
    const l = labelOf(it);
    fill(side,
      h('h2', null, it.name),
      h('div', null, h('span', { class: 'chip ' + l }, labelText(it))),
      h('p', { class: 'why' }, it.note && l !== 'perfect' ? capital(it.note) + '. ' + labelWhy(it) : labelWhy(it)),
      h('dl', null,
        h('dt', null, 'Date'), h('dd', null, dayTime(it.mtime)),
        h('dt', null, 'Size'), h('dd', null, size(it.size)),
        h('dt', null, 'Found'), h('dd', null, { named: it.deleted ? 'Deleted, with its name' : 'With its name', lost: 'In a folder a format cut off', found: 'By what is inside it' }[it.cat] || ''),
        (it.dir || []).length > 1 ? [h('dt', null, 'Folder'), h('dd', null, it.dir.slice(1).join(' / '))] : null,
        it.saved && it.saved !== '-' ? [h('dt', null, 'Saved as'), h('dd', null, it.saved)] : null),
      h('span', { class: 'grow' }),
      h('button', { class: 'btn', type: 'button', onclick: () => { toggle(it); show(); } }, S.sel.has(it.id) ? 'Selected' : 'Select'),
      h('button', { class: 'btn primary', type: 'button', onclick: () => { close(); save({ ids: [it.id] }, 'Save ' + it.name); } }, 'Save this file'));
    stage.replaceChildren(
      cur > 0 ? h('button', { class: 'nav prev', type: 'button', 'aria-label': 'Previous', onclick: () => go(-1) }, icon('chevron')) : null,
      h('div', { id: 'pv' }),
      cur < S.total - 1 ? h('button', { class: 'nav next', type: 'button', 'aria-label': 'Next', onclick: () => go(1) }, icon('chevron')) : null);
    preview(it, $('#pv', stage), () => g === gen);
  }
  document.body.append(dlg);
  dlg.showModal();
  show();
}

const none = text => h('div', { class: 'none' }, text);
async function preview(it, pv, still) {
  const url = '/api/file?id=' + it.id;
  const ext = it.ext;
  if (labelOf(it) === 'wiped') { fill(pv, none('Nothing to show: the pendrive has wiped this file.')); return; }
  if (IMAGES.has(ext)) {
    const img = h('img', { src: url, alt: 'Preview of ' + it.name });
    img.addEventListener('error', () => {
      if (THUMBS.has(ext) && img.src.indexOf('/api/thumb') < 0) img.src = '/api/thumb?id=' + it.id;
      else img.replaceWith(none('This picture cannot be shown. It may be damaged.'));
    });
    fill(pv, img);
  } else if (VIDEO.has(ext)) {
    const v = h('video', { src: url, controls: true, preload: 'metadata' });
    v.addEventListener('error', () => v.replaceWith(none('This video cannot be played here. Save it and play it with your usual video player.')));
    fill(pv, v);
  } else if (AUDIO.has(ext)) {
    const a = h('audio', { src: url, controls: true, preload: 'metadata' });
    a.addEventListener('error', () => a.replaceWith(none('This cannot be played here. Save it and open it with your usual player.')));
    fill(pv, a);
  } else if (ext === 'pdf') {
    fill(pv, h('iframe', { src: url, title: 'Preview of ' + it.name }));
  } else if (it.group === 'text' || TEXTLIKE.has(ext)) {
    fill(pv, none('Reading…'));
    const r = await api('/api/text?id=' + it.id).catch(e => ({ error: e.message }));
    if (!still()) return;
    if (r.error || !r.text) fill(pv, none(r.error ? capital(r.error) + '.' : 'This file is empty.'));
    else fill(pv, h('pre', null, r.text));
  } else {
    const kind = { heic: 'iPhone photos (HEIC)', heif: 'HEIF photos', tif: 'TIFF pictures', tiff: 'TIFF pictures', psd: 'Photoshop files', avi: 'AVI videos', wmv: 'WMV videos', mpg: 'MPEG videos', ts: 'TV recordings' }[ext];
    fill(pv, none((kind ? kind + ' cannot be shown here.' : 'No preview for this kind of file.') + ' Save it and open it on your computer or phone.'));
  }
}

// ---------------------------------------------------------------- 6. save them

async function save(sel, words) {
  const st = S.st || {};
  if (st.phase !== 'ready' && sel.all && !(await confirmBox('The pendrive is still being read', 'Files found later are not included; you can save them afterwards. Save the files found so far?', 'Save them'))) return;
  try {
    await api('/api/recover', sel);
  } catch (e) {
    messageBox('Saving could not start', e.message);
    return;
  }
  user(words);
  S.awaitDone = true;
  app(h('p', null, 'Saving them to ', h('b', null, st.outDir), '. Every file is checked as it is written; damaged photos are read again from the pendrive first.'),
    h('div', { class: 'card prog', id: 'sv' },
      h('div', { class: 'line' }, h('span', { class: 'working', id: 'sv-title' }, 'Saving'), h('span', { class: 'num', id: 'sv-pct' })),
      h('div', { class: 'meter', id: 'sv-meter' }, h('i')),
      h('div', { class: 'hint', id: 'sv-line' })));
  setComposer({ say: 'Saving files…' });
  say('Saving started');
  clearTimeout(pollTimer);
  poll();
}

function updateSaving(st) {
  const sv = st.save;
  const box = $('#sv');
  if (box && sv.running) {
    const pct = sv.total > 0 ? sv.done / sv.total : sv.files ? sv.filesDone / sv.files : 0;
    $('#sv-meter').firstChild.style.width = Math.round(pct * 100) + '%';
    $('#sv-pct').textContent = Math.floor(pct * 100) + '%';
    $('#sv-title').textContent = 'Saving ' + count(sv.files, 'file', 'files');
    $('#sv-line').textContent = fmt(sv.filesDone) + ' done, ' + size(sv.done) + ' of ' + size(sv.total);
  }
  if (S.awaitDone && !sv.running && sv.report) {
    S.awaitDone = false;
    if (box) box.remove();
    done(st);
  }
}

function done(st) {
  const sv = st.save;
  const saved = sv.good + sv.unknown + sv.damaged;
  const lines = [
    [sv.good, 'open perfectly'],
    [sv.unknown, 'could not be checked; most of these are fine'],
    [sv.damaged, 'are damaged; they are in "4 - Possibly damaged files", with the intact part and preview of damaged photos next to them'],
    [sv.rebuilt, 'were put back together from their pieces'],
    [sv.dupes, 'were left out because they are exact copies of other files'],
    [sv.erased, 'were wiped by the pendrive: nothing was left to save'],
    [sv.failed, 'could not be saved; the report says why'],
  ].filter(([n]) => n > 0);
  app(h('p', { class: 'lead' }, saved ? 'Done. ' + capital(count(saved, 'file is', 'files are')) + ' safe.' : 'Nothing new was saved.'),
    h('div', { class: 'path' }, st.outDir),
    sv.full ? note('bad', 'The drive you are saving to is full', 'Make some space on it, then save again: files already saved are skipped.') : null,
    sv.error ? note('bad', 'Saving stopped', sv.error) : null,
    lines.length ? h('ul', { class: 'tally' }, lines.map(([n, t]) => h('li', null, h('b', null, fmt(n)), h('span', null, t)))) : null,
    st.settled + st.unstable > 0 ? h('p', { class: 'hint', style: 'margin-top:14px' }, 'This pendrive is wearing out. Copy anything else you need off it, and stop using it for important files.') : null);
  refreshSummary();
  refreshConds();
  setComposer({
    say: 'Your files are in the folder above.',
    primary: { label: 'Open the folder', on: () => api('/api/open', { what: 'out' }).catch(() => {}) },
    secondary: [{ label: 'Open the report', on: () => window.open('/api/report', '_blank', 'noopener') }],
    alt: [{ label: 'Back to the files', on: () => { S.results && S.results.scrollIntoView({ behavior: 'smooth' }); resultsComposer(); } },
      { label: 'A note for a repair shop', on: () => window.open('/api/shop', '_blank', 'noopener') }],
  });
}

// ---------------------------------------------------------------- when things go wrong

function failed(st) {
  S.view = 'failed';
  app(h('p', { class: 'lead' }, 'The recovery stopped.'), h('p', null, capital(st.error || 'Something went wrong.')),
    (st.diagnosis || []).length ? h('ul', { class: 'diag card pad' }, st.diagnosis.map(d => h('li', { class: d.level }, h('span', { class: 'ic' }), h('div', null, h('b', null, d.title), h('p', null, d.text))))) : null);
  setComposer({
    say: st.files > 0 ? count(st.files, 'file was', 'files were') + ' found before it stopped.' : '',
    primary: { label: 'Start again', on: async () => { await api('/api/reset', {}).catch(() => {}); showStart(); } },
    secondary: st.files > 0 ? [{ label: 'Look at what was found', on: () => { S.st.phase = 'ready'; results(); } }] : [],
  });
}

async function startOver() {
  if (S.st && S.st.save && S.st.save.running) { messageBox('Files are being saved', 'Wait until saving has finished, then start over.'); return; }
  if (S.view !== 'start' && !(await confirmBox('Start over?', 'You go back to choosing a drive. Files you saved stay in their folder.', 'Start over'))) return;
  stopPolls();
  await api('/api/reset', {}).catch(() => {});
  showStart();
}

async function quit() {
  const saving = S.st && S.st.save && S.st.save.running;
  if (!(await confirmBox('Quit Pendrive Rescue?', saving ? 'Files are still being saved. If you quit now, the rest are not saved.' : 'Files you saved stay in their folder.', 'Quit'))) return;
  await api('/api/quit', {}).catch(() => {});
  gone('closed');
}

function gone(kind) {
  stopPolls();
  if (S.view === 'gone') return;
  S.view = 'gone';
  $$('dialog').forEach(d => d.remove());
  reset();
  setComposer(null);
  app(h('p', { class: 'lead' }, kind === 'denied' ? 'This tab is no longer connected to Pendrive Rescue.' : 'Pendrive Rescue is closed.'),
    h('p', null, kind === 'denied' ? 'Start Pendrive Rescue again: it opens a new tab.' : 'You can close this tab. Files you saved stay in their folder.'));
}

// ---------------------------------------------------------------- repairing photos already copied

let repairTimer = 0;
function chooseRepair() {
  openPicker({ files: false, title: 'Choose the folder with the photos', pick: 'Repair the photos in this folder',
    hint: 'Choose the folder with the photos that turn grey. Sub-folders are included. Several copies of the same photo (like "IMG_1234 - Copy.jpg") are combined into one.' },
  async folder => {
    try {
      await api('/api/repair', { folder, action: 'start' });
    } catch (e) {
      messageBox('The photos could not be checked', e.message);
      return;
    }
    user('Repair the photos in ' + folder);
    S.view = 'repair';
    $('#new').hidden = false;
    app(h('p', null, 'I follow every photo to where it goes wrong. Where a few wrong bits broke the header, I rebuild it from a good photo taken by the same phone; where there are several copies, I combine them.'),
      h('div', { class: 'card prog', id: 'rp' },
        h('div', { class: 'line' }, h('span', { class: 'working', id: 'rp-title' }, 'Checking the photos'), h('span', { class: 'num', id: 'rp-pct' })),
        h('div', { class: 'meter', id: 'rp-meter' }, h('i'))));
    setComposer({ say: 'Repairing photos…', secondary: [{ label: 'Stop', on: () => api('/api/repair', { action: 'stop' }).catch(() => {}) }] });
    pollRepair();
  });
}

const REPAIR = { fine: ['perfect', 'Opens fine'], repaired: ['perfect', 'Repaired'], partial: ['partly', 'Intact part saved'], preview: ['preview', 'Only the preview'], failed: ['damaged', 'Cannot be repaired'] };

async function pollRepair() {
  clearTimeout(repairTimer);
  if (S.view !== 'repair') return;
  let st;
  try { st = await api('/api/repair'); } catch (e) { repairTimer = setTimeout(pollRepair, 1000); return; }
  if (st.none) return;
  const pct = st.total ? st.done / st.total : 0;
  if ($('#rp-meter')) {
    $('#rp-meter').firstChild.style.width = Math.round(pct * 100) + '%';
    $('#rp-pct').textContent = fmt(st.done) + ' of ' + fmt(st.total);
  }
  if (st.running) { repairTimer = setTimeout(pollRepair, 600); return; }
  const card = $('#rp');
  if (card) card.remove();
  const bad = (st.photos || []).filter(p => p.status !== 'fine');
  app(st.error ? h('p', null, st.error) : h('p', { class: 'lead' }, list([
    st.fine ? count(st.fine, 'photo opens', 'photos open') + ' fine' : '',
    st.fixed ? count(st.fixed, 'photo was', 'photos were') + ' repaired' : '',
    st.partial ? count(st.partial, 'photo is', 'photos are') + ' only partly saved' : '',
    st.failed ? count(st.failed, 'photo cannot', 'photos cannot') + ' be repaired' : '',
  ]) + '.'),
  st.out ? h('div', { class: 'path' }, st.out) : null,
  bad.length ? h('div', { class: 'card' }, h('ul', { class: 'rows' }, bad.map(p => {
    const [cls, label] = REPAIR[p.status] || ['damaged', p.status];
    return h('li', { style: 'grid-template-columns:auto 1fr auto;cursor:default' }, icon('photos'),
      h('span', { class: 'nm' }, h('b', null, p.name), h('span', null, (p.status === 'partial' ? Math.round(p.share * 100) + '% of the picture. ' : '') + (p.note ? capital(p.note) + '.' : ''))),
      h('span', { class: 'chip ' + cls }, label));
  }))) : null,
  st.partial + st.failed > 0 ? h('p', { class: 'hint', style: 'margin-top:14px' },
    'Photos that are only partly saved can often be recovered whole from the pendrive itself, because I read the damaged parts there several times. Choose the pendrive on the first screen.') : null);
  setComposer({
    say: 'The results are in the folder "Repaired by Pendrive Rescue".',
    primary: st.error ? null : { label: 'Open the results folder', on: () => api('/api/repair', { action: 'open' }).catch(() => {}) },
    secondary: [{ label: 'Back to the start', on: showStart }],
  });
}

// ---------------------------------------------------------------- start

async function boot() {
  $('#quit').addEventListener('click', quit);
  $('#new').addEventListener('click', startOver);
  try { S.hello = await api('/api/hello'); } catch (e) { return; }
  let st = { phase: 'none' };
  if (S.hello.session) st = await api('/api/status').catch(() => st);
  if (st.phase === 'none') { showStart(); return; }
  reset();
  S.st = st;
  S.pick = { path: '', name: st.drive, desc: st.drive, size: st.driveSize, device: true };
  app(h('p', null, 'Welcome back. This is the recovery of ' + st.drive + '.'));
  if (st.phase === 'failed') failed(st);
  else if (st.phase === 'ready') results();
  else reading();
  if (st.save && st.save.running) S.awaitDone = true;
  poll();
}

boot();
