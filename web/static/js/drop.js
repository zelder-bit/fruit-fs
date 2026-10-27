// Drop: send files and notes between open tabs on different devices.
import { h, $, toast, fmtSize, openModal, closeModal } from './ui.js';

let ws = null;
let me = null;
let peers = [];
let target = null;           // peer chosen for the file picker
const outgoing = new Map();  // transfer id -> {files, peer}
let retryDelay = 1000;

export function initDrop() {
  $('#drop-pick').addEventListener('change', (e) => {
    if (target && e.target.files.length) offer(target, [...e.target.files]);
    e.target.value = '';
  });
  connect();
}

function connect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(`${proto}//${location.host}/api/drop/ws`);
  ws.onopen = () => { retryDelay = 1000; };
  ws.onmessage = (e) => { try { handle(JSON.parse(e.data)); } catch {} };
  ws.onclose = () => {
    peers = [];
    render();
    setTimeout(connect, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 30000);
  };
}

function send(m) { if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(m)); }

function handle(m) {
  switch (m.type) {
    case 'hello':
      me = m.you;
      $('#me-name').textContent = me.name;
      break;
    case 'peers':
      peers = m.peers || [];
      render();
      break;
    case 'text':
      showNote(m.from, m.text);
      break;
    case 'offer':
      showOffer(m);
      break;
    case 'offered':
      break;
    case 'answer':
      upload(m.transfer, m.save);
      break;
    case 'cancel': {
      const o = outgoing.get(m.transfer);
      if (o) { toast(`${o.peer.name}: ${m.error}`, 'bad'); outgoing.delete(m.transfer); setBusy(o.peer.id, null); }
      if ($('#modal').dataset.transfer === m.transfer) closeModal();
      break;
    }
    case 'done': {
      const o = outgoing.get(m.transfer);
      if (o) { toast(`Delivered to ${o.peer.name} 🍌`, 'good'); outgoing.delete(m.transfer); setBusy(o.peer.id, null); }
      break;
    }
    case 'error':
      toast(m.error, 'bad');
      break;
  }
}

const deviceEmoji = { phone: '📱', tablet: '📲', laptop: '💻' };

function render() {
  const box = $('#peers');
  if (!peers.length) {
    box.replaceChildren(h('div', { class: 'empty small-empty' },
      h('img', { src: 'svg/banana.svg', alt: '', class: 'waiting-banana' }),
      h('p', { class: 'muted' }, 'Waiting for another device…')));
    return;
  }
  box.replaceChildren(...peers.map((p) => {
    let pressTimer;
    const bubble = h('button', {
      class: 'peer', dataset: { id: p.id },
      onclick: () => { target = p; $('#drop-pick').click(); },
      oncontextmenu: (e) => { e.preventDefault(); writeNote(p); },
      onpointerdown: () => { pressTimer = setTimeout(() => { pressTimer = 'fired'; writeNote(p); }, 600); },
      onpointerup: (e) => { if (pressTimer === 'fired') e.preventDefault(); clearTimeout(pressTimer); },
      onpointerleave: () => clearTimeout(pressTimer),
    },
      h('span', { class: 'peer-fruit' }, h('img', { src: `svg/${fruitOf(p.name)}.svg`, alt: '' }), h('span', { class: 'device' }, deviceEmoji[p.device] || '💻')),
      h('span', { class: 'peer-name' }, p.name),
      h('span', { class: 'peer-user muted small' }, p.user),
      h('span', { class: 'peer-status small' }));
    return bubble;
  }));
  // Allow dropping files straight onto a bubble.
  for (const el of box.querySelectorAll('.peer')) {
    el.addEventListener('dragover', (e) => { e.preventDefault(); e.stopPropagation(); el.classList.add('target'); });
    el.addEventListener('dragleave', () => el.classList.remove('target'));
    el.addEventListener('drop', (e) => {
      e.preventDefault(); e.stopPropagation(); el.classList.remove('target');
      const p = peers.find((x) => x.id === el.dataset.id);
      if (p && e.dataTransfer.files.length) offer(p, [...e.dataTransfer.files]);
    });
  }
}

// Pick the icon by the fruit in the device's name.
function fruitOf(name) {
  const f = name.split(' ')[1]?.toLowerCase();
  const map = { orange: 'logo', banana: 'banana', kiwi: 'kiwi', lemon: 'lemon', lime: 'lemon', peach: 'peach', apricot: 'peach', grape: 'grapes', melon: 'watermelon', cherry: 'strawberry', plum: 'grapes', mango: 'peach', papaya: 'orange-slice', guava: 'apple', fig: 'coconut', pear: 'apple' };
  return map[f] || 'apple';
}

function setBusy(peerId, text) {
  const el = document.querySelector(`.peer[data-id="${CSS.escape(peerId)}"] .peer-status`);
  if (el) el.textContent = text || '';
}

function offer(peer, files) {
  const transferFiles = files.map((f) => ({ name: f.name, size: f.size, type: f.type }));
  // Remember the files until the server tells us the transfer id.
  const pending = { peer, files };
  const onOffered = (e) => {
    const m = JSON.parse(e.data);
    if (m.type === 'offered' && m.to === peer.id) {
      outgoing.set(m.transfer, pending);
      ws.removeEventListener('message', onOffered);
    }
  };
  ws.addEventListener('message', onOffered);
  send({ type: 'offer', to: peer.id, files: transferFiles });
  setBusy(peer.id, 'Waiting for them to accept…');
}

async function upload(transfer, saved) {
  const o = outgoing.get(transfer);
  if (!o) return;
  const total = o.files.reduce((s, f) => s + f.size, 0);
  let doneBytes = 0;
  for (let i = 0; i < o.files.length; i++) {
    const f = o.files[i];
    try {
      await new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('POST', `/api/drop/${transfer}/${i}`);
        xhr.setRequestHeader('X-Fruit', '1');
        xhr.upload.onprogress = (e) => setBusy(o.peer.id, `Sending ${Math.round(((doneBytes + e.loaded) / Math.max(total, 1)) * 100)}%`);
        xhr.onload = () => (xhr.status < 300 ? resolve() : reject(new Error(xhr.statusText)));
        xhr.onerror = () => reject(new Error('network'));
        xhr.send(f);
      });
      doneBytes += f.size;
    } catch {
      toast(`Sending ${f.name} failed`, 'bad');
      send({ type: 'cancel', transfer });
      return;
    }
  }
  setBusy(o.peer.id, saved ? 'Saved to their fruit-fs' : null);
}

function showOffer(m) {
  const total = m.files.reduce((s, f) => s + f.size, 0);
  const answer = (accept, save = false) => {
    send({ type: 'answer', transfer: m.transfer, accept, save });
    delete $('#modal').dataset.transfer;
    closeModal();
    if (accept && !save) receive(m);
    if (accept && save) toast('Saving to “Drop inbox”…', 'good');
  };
  const box = h('div', { class: 'dialog offer' },
    h('img', { src: 'svg/orange-slice.svg', alt: '', class: 'offer-slice' }),
    h('h3', {}, `${m.from.name} wants to share`),
    h('p', { class: 'muted small' }, `${m.from.user} · ${m.files.length} file${m.files.length > 1 ? 's' : ''} · ${fmtSize(total)}`),
    h('ul', { class: 'offer-files' }, m.files.slice(0, 8).map((f) => h('li', {}, h('span', {}, f.name), h('span', { class: 'muted' }, fmtSize(f.size)))),
      m.files.length > 8 ? h('li', { class: 'muted' }, `…and ${m.files.length - 8} more`) : null),
    h('div', { class: 'actions' },
      h('button', { class: 'btn ghost', onclick: () => answer(false) }, 'Decline'),
      h('button', { class: 'btn', onclick: () => answer(true, true) }, 'Save to fruit-fs'),
      h('button', { class: 'btn primary', onclick: () => answer(true) }, 'Download')));
  const modal = openModal(box);
  modal.dataset.transfer = m.transfer;
}

// receive starts one browser download per file; each is joined to the sender's upload.
async function receive(m) {
  for (let i = 0; i < m.files.length; i++) {
    const a = h('a', { href: `/api/drop/${m.transfer}/${i}`, download: m.files[i].name });
    document.body.append(a); a.click(); a.remove();
    // Browsers drop rapid-fire downloads, so space them out a little.
    await new Promise((r) => setTimeout(r, 700));
  }
}

function writeNote(p) {
  const area = h('textarea', { rows: '5', maxlength: '60000', placeholder: 'A link, a code, a grocery list…', required: true });
  const form = h('form', { class: 'dialog' },
    h('h3', {}, `Note to ${p.name}`),
    area,
    h('div', { class: 'actions' },
      h('button', { class: 'btn ghost', type: 'button', onclick: closeModal }, 'Cancel'),
      h('button', { class: 'btn primary', type: 'submit' }, 'Send')));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    send({ type: 'text', to: p.id, text: area.value });
    closeModal();
    toast(`Note sent to ${p.name}`, 'good');
  });
  openModal(form);
  area.focus();
}

function showNote(from, text) {
  const pre = h('pre', { class: 'note' }, text);
  const copy = h('button', { class: 'btn primary', onclick: async () => {
    try { await navigator.clipboard.writeText(text); copy.textContent = 'Copied!'; }
    catch { getSelection().selectAllChildren(pre); copy.textContent = 'Selected, press copy'; }
  } }, 'Copy');
  openModal(h('div', { class: 'dialog' },
    h('h3', {}, `Note from ${from.name}`),
    pre,
    h('div', { class: 'actions' }, h('button', { class: 'btn ghost', onclick: closeModal }, 'Close'), copy)));
}
