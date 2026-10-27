// Small DOM and API helpers shared by every view.

// h builds an element. Text children become text nodes, never HTML, so file
// names and notes from other devices can't inject markup.
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export const $ = (sel, root = document) => root.querySelector(sel);

export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

// api calls the JSON API. Every request carries X-Fruit so the server knows it
// came from these pages and not from another website.
export async function api(method, url, body) {
  const res = await fetch(url, {
    method,
    headers: { 'X-Fruit': '1', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: 'same-origin',
  });
  if (res.status === 401 && !url.endsWith('/api/me') && !url.endsWith('/api/login')) {
    location.reload();
  }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch {}
    throw new ApiError(res.status, msg);
  }
  return res.status === 204 ? null : res.json();
}

export function toast(text, kind = '') {
  const t = h('div', { class: `toast ${kind}` }, text);
  $('#toasts').append(t);
  setTimeout(() => t.classList.add('out'), 3800);
  setTimeout(() => t.remove(), 4300);
}

export function fmtSize(n) {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return `${n.toFixed(n < 10 ? 1 : 0)} ${units[i]}`;
}

export function fmtDate(iso) {
  const d = new Date(iso);
  const sameYear = d.getFullYear() === new Date().getFullYear();
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', ...(sameYear ? {} : { year: 'numeric' }) });
}

// Which fruit stands for which kind of file.
const KINDS = {
  strawberry: 'jpg jpeg png gif webp bmp svg heic heif avif tif tiff ico',
  watermelon: 'mp4 mkv mov webm avi m4v wmv flv',
  grapes: 'mp3 m4a flac wav ogg opus aac wma',
  lemon: 'txt md rtf doc docx odt csv xls xlsx ods ppt pptx odp log json yaml yml toml ini conf',
  peach: 'pdf epub',
  coconut: 'zip tar gz tgz xz zst 7z rar bz2 iso dmg',
  kiwi: 'go py js ts jsx tsx c h cpp rs java kt sh html css xml sql rb php swift',
};
const EXT_FRUIT = {};
for (const [fruit, exts] of Object.entries(KINDS)) for (const e of exts.split(' ')) EXT_FRUIT[e] = fruit;

export function ext(name) {
  const i = name.lastIndexOf('.');
  return i > 0 ? name.slice(i + 1).toLowerCase() : '';
}

export function fruitFor(entry) {
  if (entry.dir) return 'crate';
  return EXT_FRUIT[ext(entry.name)] || 'apple';
}

export function icon(fruit, cls = 'fruit') {
  return h('img', { src: `svg/${fruit}.svg`, alt: '', class: cls, draggable: 'false' });
}

// Preview kinds the server will show inline.
export function previewKind(name) {
  const f = EXT_FRUIT[ext(name)];
  const e = ext(name);
  if (f === 'strawberry' && !['heic', 'heif', 'tif', 'tiff'].includes(e)) return 'image';
  if (f === 'watermelon') return 'video';
  if (f === 'grapes') return 'audio';
  if (e === 'pdf') return 'pdf';
  if (f === 'lemon' || f === 'kiwi') {
    if (['doc', 'docx', 'odt', 'xls', 'xlsx', 'ods', 'ppt', 'pptx', 'odp', 'rtf'].includes(e)) return null;
    return 'text';
  }
  return null;
}

export const joinPath = (dir, name) => (dir === '' || dir === '.' ? name : `${dir}/${name}`);

export function dlURL(path, inline = false) {
  return `/api/dl?path=${encodeURIComponent(path)}${inline ? '&inline=1' : ''}`;
}

// --- modal dialogs ---------------------------------------------------------

export function openModal(content, { wide = false, onClose } = {}) {
  const m = $('#modal');
  m.replaceChildren(content);
  m.className = wide ? 'wide' : '';
  m.onclose = () => { m.replaceChildren(); onClose?.(); };
  if (!m.open) m.showModal();
  return m;
}

export function closeModal() {
  const m = $('#modal');
  if (m.open) m.close();
}

// ask shows a small form; resolves to the entered value or null.
export function ask({ title, label, value = '', ok = 'OK', danger = false, input = true, allowEmpty = false }) {
  return new Promise((resolve) => {
    let result = null;
    const field = input ? h('input', { type: 'text', value, required: !allowEmpty, spellcheck: 'false' }) : null;
    const form = h('form', { class: 'dialog', method: 'dialog' },
      h('h3', {}, title),
      input ? h('label', { class: 'field' }, h('span', {}, label), field) : h('p', {}, label),
      h('div', { class: 'actions' },
        h('button', { class: 'btn ghost', type: 'button', onclick: () => closeModal() }, 'Cancel'),
        h('button', { class: `btn ${danger ? 'danger' : 'primary'}`, type: 'submit' }, ok)));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      result = input ? field.value.trim() : true;
      closeModal();
    });
    openModal(form, { onClose: () => resolve(result) });
    if (field) { field.focus(); const dot = value.lastIndexOf('.'); field.setSelectionRange(0, dot > 0 ? dot : value.length); }
  });
}
