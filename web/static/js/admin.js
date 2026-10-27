// Admin view: guest (LAN) sign-in settings. Only tailnet admins see the tab,
// and the server refuses everyone else regardless.
import { h, $, api, toast } from './ui.js';

const PRESETS = [5, 10, 30, 60, 240, 1440];

function label(min) {
  return min < 60 ? `${min} min` : min % 60 ? `${(min / 60).toFixed(1)} h` : `${min / 60} h`;
}

export async function showAdmin() {
  const box = $('#admin-body');
  let s;
  try { s = await api('GET', '/api/admin/settings'); } catch (err) { toast(err.message, 'bad'); return; }

  const enabled = h('input', { type: 'checkbox', checked: s.guestEnabled });
  const minutes = h('input', { type: 'number', min: s.minMinutes, max: s.maxMinutes, step: 1, value: s.guestMinutes, required: true });
  const presets = h('div', { class: 'presets' }, PRESETS.map((m) =>
    h('button', { type: 'button', class: 'btn small', onclick: () => { minutes.value = m; } }, label(m))));
  const save = h('button', { class: 'btn primary', type: 'submit' }, 'Save');

  const form = h('form', { class: 'admin-card' },
    h('h3', {}, 'Guests on the home network'),
    h('p', { class: 'muted' }, 'People on the LAN sign in with the home password as guests. Tailnet users are never affected.'),
    s.passwordConfigured ? null : h('p', { class: 'notice' }, 'FRUITFS_PASSWORD_HASH is not set, so nobody can sign in as a guest right now.'),
    h('label', { class: 'switch' }, enabled, h('span', {}, 'Allow guest sign-in')),
    h('label', { class: 'field' }, h('span', {}, `Guest session length in minutes (${s.minMinutes}–${s.maxMinutes})`), minutes),
    presets,
    h('p', { class: 'muted small' }, 'A new length applies to guests who sign in from now on. Turning guest sign-in off signs out current guests.'),
    h('div', { class: 'actions' }, save));

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    save.disabled = true;
    try {
      await api('PUT', '/api/admin/settings', { guestEnabled: enabled.checked, guestMinutes: Number(minutes.value) });
      toast('Settings saved', 'good');
    } catch (err) { toast(err.message, 'bad'); }
    save.disabled = false;
  });
  box.replaceChildren(form);
}
