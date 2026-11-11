// Sticky notes: jot something down and save it as a text file in the open folder.
import { h, api, ApiError, toast, joinPath, openModal, closeModal } from './ui.js';

const DRAFT_KEY = 'fruit-fs.note-draft';
const MAX_BYTES = 1024 * 1024; // a note, not a novel; keeps the save to one request

function loadDraft() {
  try { return JSON.parse(localStorage.getItem(DRAFT_KEY)) || null; } catch { return null; }
}
function saveDraft(d) { try { localStorage.setItem(DRAFT_KEY, JSON.stringify(d)); } catch {} }
function clearDraft() { try { localStorage.removeItem(DRAFT_KEY); } catch {} }

function defaultTitle() {
  const d = new Date();
  const p = (n) => String(n).padStart(2, '0');
  return `Note ${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}.${p(d.getMinutes())}`;
}

// fileName turns a title into a safe file name, adding .txt unless the title
// already ends in a plain-text extension.
export function fileName(title) {
  let name = title.replace(/[\u0000-\u001f\u007f]/g, '').replace(/[\\/]+/g, '-').trim().replace(/^\.+/, '');
  if (!name) name = defaultTitle();
  return /\.(txt|md)$/i.test(name) ? name : `${name}.txt`;
}

function numbered(name, n) {
  if (n < 2) return name;
  const dot = name.lastIndexOf('.');
  return `${name.slice(0, dot)} (${n})${name.slice(dot)}`;
}

// writeFile sends one small blob through the upload API.
async function writeFile(path, blob) {
  const up = await api('POST', '/api/uploads', { path, size: blob.size, replace: false });
  if (up.done) return;
  const res = await fetch(`/api/uploads/${up.id}`, {
    method: 'PATCH',
    headers: { 'X-Fruit': '1', 'Upload-Offset': '0', 'Content-Type': 'application/octet-stream' },
    body: blob,
    credentials: 'same-origin',
  });
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch {}
    api('DELETE', `/api/uploads/${up.id}`).catch(() => {});
    throw new ApiError(res.status, msg);
  }
}

// saveNote writes the note into dir, picking "name (2).txt" and so on rather
// than overwriting a file that is already there. Resolves to the name used.
async function saveNote(dir, name, text) {
  const blob = new Blob([text], { type: 'text/plain' });
  if (blob.size > MAX_BYTES) throw new Error('That note is too long; notes can be up to 1 MB.');
  for (let n = 1; n <= 50; n++) {
    const candidate = numbered(name, n);
    try {
      await writeFile(joinPath(dir, candidate), blob);
      return candidate;
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 409)) throw err;
    }
  }
  throw new Error('Too many notes with that name; try another title.');
}

// openNote shows the sticky note. dir is the folder to save into; onSaved runs
// after a successful save so the listing can refresh.
export function openNote(dir, onSaved) {
  const draft = loadDraft();
  const title = h('input', { type: 'text', class: 'sticky-title', value: draft?.title || defaultTitle(), maxlength: '120', spellcheck: 'false', 'aria-label': 'Note title' });
  const area = h('textarea', { class: 'sticky-text', rows: '9', placeholder: 'A link, a code, a grocery list…', 'aria-label': 'Note text' });
  area.value = draft?.text || '';
  const where = h('p', { class: 'sticky-where small' }, `Saves to ${dir ? `/${dir}` : 'Home'}`);
  const saveBtn = h('button', { class: 'btn primary', type: 'submit' }, 'Save note');
  let saved = false;

  const remember = () => { if (!saved) saveDraft({ title: title.value, text: area.value }); };
  title.addEventListener('input', remember);
  area.addEventListener('input', remember);

  const form = h('form', { class: 'dialog sticky' },
    title,
    area,
    where,
    h('div', { class: 'actions' },
      h('button', { class: 'btn ghost', type: 'button', onclick: () => { saved = true; clearDraft(); closeModal(); } }, 'Discard'),
      h('button', { class: 'btn ghost', type: 'button', onclick: closeModal }, 'Close'),
      saveBtn));

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (saveBtn.disabled) return;
    if (!area.value.trim()) { area.focus(); toast('Write something first', 'bad'); return; }
    saveBtn.disabled = true;
    try {
      const name = await saveNote(dir, fileName(title.value), area.value);
      saved = true;
      clearDraft();
      closeModal();
      toast(`Stuck "${name}" in ${dir ? dir.split('/').pop() : 'Home'}`, 'good');
      onSaved?.(name);
    } catch (err) {
      toast(err.message, 'bad');
      saveBtn.disabled = false;
    }
  });
  area.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); form.requestSubmit(); }
  });

  openModal(form);
  document.getElementById('modal').classList.add('sticky-modal');
  area.focus();
  area.setSelectionRange(area.value.length, area.value.length);
}
