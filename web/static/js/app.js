// Entry point: sign-in, theme, routing between the Files, Drop and Trash views.
import { h, $, api, toast, fmtSize, fmtDate, fruitFor, icon, ask, markGuestEnded, takeGuestEnded } from './ui.js';
import { initFiles, showFiles } from './files.js';
import { initDrop } from './drop.js';

let identity = null;

// --- theme ---------------------------------------------------------------------

function applyTheme(t) {
  if (t) document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
  const dark = t ? t === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  $('#theme').textContent = dark ? '☀︎' : '☾';
}

function initTheme() {
  let saved = null;
  try { saved = localStorage.getItem('fruit-fs.theme'); } catch {}
  applyTheme(saved);
  $('#theme').addEventListener('click', () => {
    const cur = document.documentElement.dataset.theme
      || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    const next = cur === 'dark' ? 'light' : 'dark';
    try { localStorage.setItem('fruit-fs.theme', next); } catch {}
    applyTheme(next);
  });
}

// --- sign-in -------------------------------------------------------------------

async function start() {
  initTheme();
  try {
    identity = await api('GET', '/api/me');
  } catch (err) {
    if (err.status === 401) return showLogin();
    document.body.replaceChildren(h('p', { class: 'fatal' }, `fruit-fs is unreachable: ${err.message}`));
    return;
  }
  showApp();
}

function showLogin() {
  $('#login').hidden = false;
  const form = $('#login form');
  const err = form.querySelector('.error');
  const notice = form.querySelector('.notice');
  if (takeGuestEnded()) {
    notice.textContent = 'Your guest session ended. Sign in again to keep going.';
    notice.hidden = false;
  }
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    err.hidden = true;
    notice.hidden = true;
    try {
      identity = await api('POST', '/api/login', { password: form.password.value });
      $('#login').hidden = true;
      showApp();
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
      form.password.select();
    }
  });
}

function showApp() {
  $('#app').hidden = false;
  document.documentElement.dataset.via = identity.via;
  const chip = $('#user');
  chip.replaceChildren(
    identity.pic ? h('img', { src: identity.pic, alt: '', referrerpolicy: 'no-referrer' }) : h('span', { class: 'avatar' }, (identity.name || '?')[0].toUpperCase()),
    h('span', { class: 'user-name' }, identity.name));
  chip.title = identity.via === 'tailscale' ? `Signed in through Tailscale as ${identity.user}` : 'Signed in as a guest with the home password';
  if (identity.via === 'guest') startGuestTimer(chip);
  chip.addEventListener('click', async () => {
    if (identity.via !== 'guest') { toast(chip.title); return; }
    if (await ask({ title: 'Sign out?', label: 'You will need the password to get back in.', ok: 'Sign out', input: false })) {
      await api('POST', '/api/logout');
      location.reload();
    }
  });
  initFiles();
  initDrop();
  $('#empty-trash').addEventListener('click', emptyTrash);
  addEventListener('hashchange', route);
  route();
}

// --- guest countdown -------------------------------------------------------------

// Guests see how long they have left; when the time is up they go back to the
// sign-in screen. The server refuses the session at the same moment anyway.
function startGuestTimer(chip) {
  const ends = Date.parse(identity.expiresAt);
  if (!ends) return;
  const left = h('span', { class: 'guest-left' });
  chip.append(left);
  const tick = () => {
    const ms = ends - Date.now();
    if (ms <= 0) {
      clearInterval(timer);
      markGuestEnded();
      location.reload();
      return;
    }
    const min = Math.ceil(ms / 60000);
    left.textContent = min > 90 ? `${Math.round(min / 60)} h left` : `${min} min left`;
    left.classList.toggle('low', ms <= 2 * 60000);
    chip.setAttribute('aria-label', `Guest, ${left.textContent}`);
  };
  const timer = setInterval(tick, 5000);
  tick();
}

// --- routing -------------------------------------------------------------------

function route() {
  const hash = location.hash || '#/files/';
  const tab = hash.startsWith('#/drop') ? 'drop' : hash.startsWith('#/trash') ? 'trash' : 'files';
  for (const v of document.querySelectorAll('.view')) v.hidden = v.id !== `view-${tab}`;
  for (const a of document.querySelectorAll('.tabs a')) a.classList.toggle('active', a.dataset.tab === tab);
  if (tab === 'files') {
    const raw = hash.replace(/^#\/files\/?/, '');
    let path = '';
    try { path = raw.split('/').map(decodeURIComponent).join('/'); } catch {}
    showFiles(path);
  }
  if (tab === 'trash') showTrash();
}

// --- trash ---------------------------------------------------------------------

async function showTrash() {
  const box = $('#trash-list');
  try {
    const { items } = await api('GET', '/api/trash');
    if (!items.length) {
      box.replaceChildren(h('div', { class: 'empty' }, h('img', { src: 'svg/empty.svg', alt: '' }), h('h3', {}, 'Nothing composting'), h('p', { class: 'muted' }, 'Deleted files land here.')));
      return;
    }
    box.replaceChildren(...items.map((it) => {
      const name = it.orig.split('/').pop();
      return h('div', { class: 'list-row' },
        icon(fruitFor({ name, dir: it.dir }), 'fruit small'),
        h('div', { class: 'grow' },
          h('div', { class: 'name' }, name),
          h('div', { class: 'muted small' }, `from /${it.orig} · deleted ${fmtDate(it.deleted)}${it.dir ? '' : ` · ${fmtSize(it.size)}`}`)),
        h('button', { class: 'btn small', onclick: () => restore(it) }, 'Restore'));
    }));
  } catch (err) { toast(err.message, 'bad'); }
}

async function restore(it) {
  try {
    await api('POST', '/api/trash/restore', { id: it.id });
    toast(`Restored /${it.orig}`, 'good');
  } catch (err) { toast(err.message, 'bad'); }
  showTrash();
}

async function emptyTrash() {
  if (!await ask({ title: 'Empty the compost bin?', label: 'Everything in Trash is deleted for good.', ok: 'Empty', danger: true, input: false })) return;
  await api('POST', '/api/trash/empty');
  showTrash();
}

start();
