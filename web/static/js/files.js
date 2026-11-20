// The file browser: listing, navigation, selection, actions and previews.
import { h, $, api, toast, fmtSize, fmtDate, fruitFor, icon, previewKind, joinPath, dlURL, openModal, closeModal, ask } from './ui.js';
import { enqueue, filesFromDrop, setUploadDoneHandler } from './upload.js';
import { openNote, writeFile } from './notes.js';

let cwd = '';          // current folder, '' = top
let entries = [];      // what's shown right now
let searching = false;
const selected = new Set();
let layout = load('layout', 'grid');

function load(key, def) { try { return localStorage.getItem(`fruit-fs.${key}`) || def; } catch { return def; } }
function save(key, v) { try { localStorage.setItem(`fruit-fs.${key}`, v); } catch {} }

export function initFiles() {
  $('#pick-files').addEventListener('change', (e) => { uploadPicked(e.target.files); e.target.value = ''; });
  $('#pick-folder').addEventListener('change', (e) => { uploadPicked(e.target.files); e.target.value = ''; });
  $('#new-folder').addEventListener('click', newFolder);
  $('#new-note').addEventListener('click', () => openNote(cwd, () => { if (searching) return; refresh(); loadStats(); }));
  $('#layout').addEventListener('click', () => { layout = layout === 'grid' ? 'list' : 'grid'; save('layout', layout); render(); });

  let timer;
  $('#search').addEventListener('input', (e) => {
    clearTimeout(timer);
    timer = setTimeout(() => search(e.target.value.trim()), 250);
  });

  document.querySelector('.selection-bar').addEventListener('click', (e) => {
    const act = e.target.closest('[data-act]')?.dataset.act;
    const paths = [...selected];
    if (act === 'zip') zip(paths);
    if (act === 'move') moveMany(paths);
    if (act === 'delete') remove(paths);
    if (act === 'clear') { selected.clear(); render(); }
  });

  setupDropzone();
  setUploadDoneHandler(() => { if (location.hash.startsWith('#/files')) refresh(); });
}

export function showFiles(path) {
  if (path !== cwd) selected.clear();
  cwd = path;
  $('#search').value = '';
  searching = false;
  refresh();
  loadStats();
}

async function refresh() {
  try {
    const res = await api('GET', `/api/ls?path=${encodeURIComponent(cwd)}`);
    entries = res.entries.map((e) => ({ ...e, path: joinPath(cwd, e.name) }));
  } catch (err) {
    entries = [];
    toast(err.message, 'bad');
  }
  render();
}

async function search(q) {
  if (!q) { searching = false; return refresh(); }
  searching = true;
  const res = await api('GET', `/api/search?q=${encodeURIComponent(q)}`);
  entries = res.results;
  render();
}

async function loadStats() {
  try {
    const { total, free } = await api('GET', '/api/stats');
    if (!total) return;
    const used = total - free;
    $('.disk-bar > div').style.setProperty('--p', `${(used / total) * 100}%`);
    $('.disk span').textContent = `${fmtSize(free)} free of ${fmtSize(total)}`;
  } catch {}
}

// --- rendering ---------------------------------------------------------------

function renderCrumbs() {
  const parts = cwd ? cwd.split('/') : [];
  const crumbs = [h('a', { href: '#/files/' }, icon('crate', 'crumb-icon'), 'Home')];
  parts.forEach((p, i) => {
    crumbs.push(h('span', { class: 'sep' }, '›'));
    crumbs.push(h('a', { href: `#/files/${parts.slice(0, i + 1).map(encodeURIComponent).join('/')}` }, p));
  });
  if (searching) crumbs.push(h('span', { class: 'sep' }, '›'), h('span', {}, 'search results'));
  $('.crumbs').replaceChildren(...crumbs);
}

function render() {
  renderCrumbs();
  const list = $('#listing');
  list.className = layout;
  $('#layout').textContent = layout === 'grid' ? '☰' : '▦';

  if (!entries.length) {
    list.replaceChildren(h('div', { class: 'empty' },
      h('img', { src: 'svg/empty.svg', alt: '' }),
      h('h3', {}, searching ? 'No fruit by that name' : 'This crate is empty'),
      h('p', { class: 'muted' }, searching ? 'Try a shorter search.' : 'Drag files here, or use Upload.')));
  } else {
    list.replaceChildren(...entries.map(card));
  }
  const bar = document.querySelector('.selection-bar');
  bar.hidden = selected.size === 0;
  bar.querySelector('.count').textContent = `${selected.size} picked`;
}

function card(e) {
  const fruit = fruitFor(e);
  const isImage = !e.dir && previewKind(e.name) === 'image';
  const thumb = isImage
    ? h('img', { src: dlURL(e.path, true), alt: '', loading: 'lazy', class: 'thumb', onerror: (ev) => ev.target.replaceWith(icon(fruit)) })
    : icon(fruit);
  const check = h('input', {
    type: 'checkbox', class: 'pick', 'aria-label': `Select ${e.name}`, checked: selected.has(e.path),
    onclick: (ev) => ev.stopPropagation(),
    onchange: (ev) => { ev.target.checked ? selected.add(e.path) : selected.delete(e.path); render(); },
  });
  const meta = e.dir ? 'Crate' : fmtSize(e.size);
  const el = h('div', {
    class: `card ${selected.has(e.path) ? 'selected' : ''} ${e.dir ? 'is-dir' : ''}`, tabindex: '0', role: 'button',
    onclick: () => open(e),
    onkeydown: (ev) => { if (ev.key === 'Enter') open(e); },
    oncontextmenu: (ev) => { ev.preventDefault(); menu(e, ev); },
  },
    check,
    h('div', { class: 'thumb-wrap' }, thumb),
    h('div', { class: 'card-text' },
      h('div', { class: 'name', title: e.name }, e.name),
      h('div', { class: 'meta muted small' }, searching ? (e.path.includes('/') ? e.path.slice(0, e.path.lastIndexOf('/')) : 'Home') : `${meta} · ${fmtDate(e.mtime)}`)),
    h('button', { class: 'icon-btn more', 'aria-label': `Actions for ${e.name}`, onclick: (ev) => { ev.stopPropagation(); menu(e, ev); } }, '⋯'));

  if (e.dir) {
    el.addEventListener('dragover', (ev) => { if (ev.dataTransfer.types.includes('Files')) { ev.preventDefault(); ev.stopPropagation(); el.classList.add('target'); } });
    el.addEventListener('dragleave', () => el.classList.remove('target'));
    el.addEventListener('drop', async (ev) => {
      ev.preventDefault(); ev.stopPropagation(); el.classList.remove('target'); hideDropzone();
      const files = await filesFromDrop(ev.dataTransfer);
      enqueue(files.map(({ file, rel }) => ({ file, path: joinPath(e.path, rel) })));
    });
  }
  return el;
}

function open(e) {
  if (e.dir) location.hash = `#/files/${e.path.split('/').map(encodeURIComponent).join('/')}`;
  else preview(e);
}

// --- context menu --------------------------------------------------------------

function menu(e, ev) {
  document.querySelector('.menu')?.remove();
  const item = (label, fn, cls = '') => h('button', { class: cls, onclick: () => { m.remove(); fn(); } }, label);
  const m = h('div', { class: 'menu', role: 'menu' },
    e.dir ? item('Open', () => open(e)) : item('Preview', () => preview(e)),
    e.dir ? item('Download as zip', () => zip([e.path])) : item('Download', () => download(e.path)),
    item('Rename', () => rename(e)),
    item('Move', () => moveMany([e.path])),
    item('Delete', () => remove([e.path]), 'danger'));
  document.body.append(m);
  const r = m.getBoundingClientRect();
  const x = Math.min(ev.clientX, innerWidth - r.width - 8);
  const y = Math.min(ev.clientY, innerHeight - r.height - 8);
  m.style.setProperty('left', `${Math.max(8, x)}px`);
  m.style.setProperty('top', `${Math.max(8, y)}px`);
  setTimeout(() => document.addEventListener('click', () => m.remove(), { once: true }));
}

// --- actions -------------------------------------------------------------------

function download(path) {
  const a = h('a', { href: dlURL(path), download: '' });
  document.body.append(a); a.click(); a.remove();
}

function zip(paths) {
  const q = paths.map((p) => `path=${encodeURIComponent(p)}`).join('&');
  const a = h('a', { href: `/api/zip?${q}`, download: '' });
  document.body.append(a); a.click(); a.remove();
}

async function newFolder() {
  const name = await ask({ title: 'New crate', label: 'Folder name', ok: 'Create' });
  if (!name) return;
  try {
    await api('POST', '/api/mkdir', { path: joinPath(cwd, name) });
    refresh();
  } catch (err) { toast(err.message, 'bad'); }
}

async function rename(e) {
  const name = await ask({ title: 'Rename', label: 'New name', value: e.name, ok: 'Rename' });
  if (!name || name === e.name) return;
  const parent = e.path.includes('/') ? e.path.slice(0, e.path.lastIndexOf('/')) : '';
  try {
    await api('POST', '/api/move', { from: e.path, to: joinPath(parent, name) });
    refresh();
  } catch (err) { toast(err.message, 'bad'); }
}

async function moveMany(paths) {
  const dest = await ask({ title: `Move ${paths.length > 1 ? `${paths.length} items` : 'item'}`, label: 'Into folder (blank = Home)', value: cwd, ok: 'Move', allowEmpty: true });
  if (dest === null) return;
  let ok = 0;
  for (const p of paths) {
    try {
      await api('POST', '/api/move', { from: p, to: joinPath(dest.replace(/^\/+|\/+$/g, ''), p.split('/').pop()) });
      ok++;
    } catch (err) { toast(`${p.split('/').pop()}: ${err.message}`, 'bad'); }
  }
  if (ok) toast(`Moved ${ok} item${ok > 1 ? 's' : ''}`, 'good');
  selected.clear();
  refresh();
}

async function remove(paths) {
  const label = paths.length === 1 ? `"${paths[0].split('/').pop()}"` : `${paths.length} items`;
  const yes = await ask({ title: 'Toss in the compost?', label: `${label} will wait in Trash for 30 days.`, ok: 'Delete', danger: true, input: false });
  if (!yes) return;
  try {
    await api('POST', '/api/delete', { paths });
    toast(`Moved ${label} to Trash`, 'good');
  } catch (err) { toast(err.message, 'bad'); }
  selected.clear();
  refresh();
}

function uploadPicked(fileList) {
  const items = [...fileList].map((file) => ({ file, path: joinPath(cwd, file.webkitRelativePath || file.name) }));
  if (items.length) enqueue(items);
}

// --- drag and drop onto the page -------------------------------------------------

let dragDepth = 0;
function hideDropzone() { dragDepth = 0; $('#dropzone').hidden = true; }

function setupDropzone() {
  const active = () => location.hash.startsWith('#/files') || location.hash === '';
  addEventListener('dragenter', (e) => {
    if (!active() || !e.dataTransfer.types.includes('Files')) return;
    dragDepth++;
    $('#dropzone').hidden = false;
  });
  addEventListener('dragleave', () => { if (--dragDepth <= 0) hideDropzone(); });
  addEventListener('dragover', (e) => { if (active() && e.dataTransfer.types.includes('Files')) e.preventDefault(); });
  addEventListener('drop', async (e) => {
    if (!active()) return;
    e.preventDefault();
    hideDropzone();
    const files = await filesFromDrop(e.dataTransfer);
    enqueue(files.map(({ file, rel }) => ({ file, path: joinPath(cwd, rel) })));
  });
}

// --- preview ----------------------------------------------------------------------

const EDIT_MAX = 524288; // the preview loads at most this much, so only whole files are editable

function preview(e) {
  const files = entries.filter((x) => !x.dir);
  let i = files.findIndex((x) => x.path === e.path);
  const body = h('div', { class: 'preview-body' });
  const title = h('div', { class: 'name' });
  const meta = h('div', { class: 'muted small' });
  const dl = h('a', { class: 'btn primary small', download: '' }, 'Download');
  const editBtn = h('button', { class: 'btn small', type: 'button', hidden: true }, 'Edit');
  const cancelBtn = h('button', { class: 'btn ghost small', type: 'button', hidden: true }, 'Cancel');
  let text = '';        // what the file holds right now, as far as we know
  let lastMod = null;   // its Last-Modified when we read it
  let area = null;      // the editor while editing
  let dirty = false;
  let saving = false;

  const okToLeave = () => !dirty || confirm('Discard your unsaved changes?');
  const showMeta = (f) => { meta.textContent = `${fmtSize(f.size)} · ${fmtDate(f.mtime)}`; };
  const stopEdit = () => {
    area = null; dirty = false;
    editBtn.textContent = 'Edit';
    cancelBtn.hidden = true;
    body.replaceChildren(h('pre', {}, text));
  };
  const startEdit = () => {
    area = h('textarea', { class: 'editor', spellcheck: 'false', 'aria-label': `Edit ${files[i].name}` });
    area.value = text;
    area.addEventListener('input', () => { dirty = true; });
    area.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && (ev.ctrlKey || ev.metaKey)) { ev.preventDefault(); saveEdit(); }
    });
    body.replaceChildren(area);
    editBtn.textContent = 'Save';
    cancelBtn.hidden = false;
    area.focus();
  };
  const modified = async (f) => {
    const res = await fetch(dlURL(f.path, true), { method: 'HEAD', cache: 'no-store' });
    if (!res.ok) throw new Error("Couldn't check the file; is it still there?");
    return res.headers.get('Last-Modified');
  };
  const saveEdit = async () => {
    if (saving || !area) return;
    const f = files[i];
    saving = true;
    editBtn.disabled = true;
    area.readOnly = true; // what's on screen is what gets saved
    try {
      // shortcut: Last-Modified has 1-second precision, so a change made in the same second as our read slips past; add an ETag/If-Match check on the server if that matters.
      if (await modified(f) !== lastMod) throw new Error('This file changed since you opened it. Copy your edits, then close and reopen it.');
      const value = area.value;
      // Textareas turn CRLF into LF; put Windows line endings back if the file had them.
      const blob = new Blob([text.includes('\r\n') ? value.replace(/\n/g, '\r\n') : value], { type: 'text/plain' });
      await writeFile(f.path, blob, true);
      text = value;
      f.size = blob.size;
      f.mtime = new Date().toISOString();
      showMeta(f);
      lastMod = await modified(f).catch(() => null);
      editBtn.hidden = !lastMod; // can't check for conflicts without it
      stopEdit();
      toast(`Saved "${f.name}"`, 'good');
      if (!searching) refresh();
    } catch (err) {
      toast(err.message, 'bad');
    } finally {
      saving = false;
      editBtn.disabled = false;
      if (area) area.readOnly = false;
    }
  };
  editBtn.addEventListener('click', () => (area ? saveEdit() : startEdit()));
  cancelBtn.addEventListener('click', () => { if (okToLeave()) stopEdit(); });

  const show = async () => {
    const f = files[i];
    area = null; dirty = false; lastMod = null;
    editBtn.hidden = true;
    editBtn.textContent = 'Edit';
    cancelBtn.hidden = true;
    title.textContent = f.name;
    showMeta(f);
    dl.href = dlURL(f.path);
    const kind = previewKind(f.name);
    const src = dlURL(f.path, true);
    if (kind === 'image') body.replaceChildren(h('img', { src, alt: f.name }));
    else if (kind === 'video') body.replaceChildren(h('video', { src, controls: true, autoplay: true, playsinline: true }));
    else if (kind === 'audio') body.replaceChildren(h('div', { class: 'audio' }, icon('grapes', 'fruit big'), h('audio', { src, controls: true, autoplay: true })));
    else if (kind === 'pdf') body.replaceChildren(h('iframe', { src, title: f.name }));
    else if (kind === 'text') {
      body.replaceChildren(h('p', { class: 'muted' }, 'Squeezing…'));
      try {
        const res = await fetch(src, { headers: { Range: `bytes=0-${EDIT_MAX - 1}` }, cache: 'no-store' });
        const buf = await res.arrayBuffer();
        if (files[i] !== f) return; // stepped to another file meanwhile
        let utf8 = true;
        try { text = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(buf); }
        catch { utf8 = false; text = new TextDecoder().decode(buf); }
        lastMod = res.headers.get('Last-Modified');
        // Only offer editing when we hold the whole file as valid UTF-8, so saving can't truncate or mangle it.
        // Check the response itself: the listing's size may be stale.
        const total = res.status === 206 ? Number(res.headers.get('Content-Range')?.split('/')[1]) : buf.byteLength;
        editBtn.hidden = !(res.ok && utf8 && total === buf.byteLength && lastMod);
        body.replaceChildren(h('pre', {}, text + (f.size > EDIT_MAX ? '\n\n… (only the first 512 KB is shown)' : '')));
      } catch { if (files[i] === f) body.replaceChildren(h('p', {}, "Couldn't read it.")); }
    } else {
      body.replaceChildren(h('div', { class: 'no-preview' }, icon(fruitFor(f), 'fruit big'), h('p', {}, 'No preview for this one — download it instead.')));
    }
  };
  const step = (d) => { if (files.length > 1 && !saving && okToLeave()) { i = (i + d + files.length) % files.length; show(); } };
  const keys = (ev) => {
    if (area) return; // arrows move the cursor while editing
    if (ev.key === 'ArrowRight') step(1);
    if (ev.key === 'ArrowLeft') step(-1);
  };
  const onEscape = (ev) => { if (saving || !okToLeave()) ev.preventDefault(); };
  const onUnload = (ev) => { if (dirty) ev.preventDefault(); };

  const view = h('div', { class: 'preview' },
    h('div', { class: 'preview-head' },
      h('div', { class: 'preview-title' }, title, meta),
      cancelBtn,
      editBtn,
      dl,
      h('button', { class: 'icon-btn', 'aria-label': 'Close', onclick: () => { if (!saving && okToLeave()) closeModal(); } }, '✕')),
    h('div', { class: 'preview-stage' },
      files.length > 1 ? h('button', { class: 'nav prev', 'aria-label': 'Previous', onclick: () => step(-1) }, '‹') : null,
      body,
      files.length > 1 ? h('button', { class: 'nav next', 'aria-label': 'Next', onclick: () => step(1) }, '›') : null));
  addEventListener('keydown', keys);
  addEventListener('beforeunload', onUnload);
  const modal = openModal(view, {
    wide: true,
    onClose: () => {
      removeEventListener('keydown', keys);
      removeEventListener('beforeunload', onUnload);
      modal.removeEventListener('cancel', onEscape);
    },
  });
  modal.addEventListener('cancel', onEscape);
  show();
}
