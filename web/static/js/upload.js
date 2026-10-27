// Upload queue: sends files in 8 MB chunks and resumes after a dropped connection.
import { h, $, api, ApiError, fmtSize, toast } from './ui.js';

const CHUNK = 8 * 1024 * 1024;
const PARALLEL = 2;
const queue = [];
let running = 0;
let onDone = () => {};

export function setUploadDoneHandler(fn) { onDone = fn; }

// enqueue takes [{file, path}] where path is the destination inside the data dir.
export function enqueue(items) {
  for (const it of items) {
    const job = { ...it, sent: 0, state: 'waiting', row: null, xhr: null, id: null };
    job.row = renderRow(job);
    queue.push(job);
  }
  $('#tray').hidden = false;
  pump();
}

function pump() {
  while (running < PARALLEL) {
    const job = queue.find((j) => j.state === 'waiting');
    if (!job) break;
    running++;
    job.state = 'sending';
    run(job).finally(() => { running--; pump(); maybeFinished(); });
  }
}

function maybeFinished() {
  if (queue.some((j) => j.state === 'waiting' || j.state === 'sending')) return;
  const ok = queue.filter((j) => j.state === 'done').length;
  if (ok) toast(`${ok} file${ok > 1 ? 's' : ''} picked and packed 🍊`, 'good');
  onDone();
  // Tidy finished rows after a moment; keep failed ones so they can be retried.
  setTimeout(() => {
    for (const j of queue.filter((j) => j.state === 'done' || j.state === 'cancelled')) {
      j.row.remove();
      queue.splice(queue.indexOf(j), 1);
    }
    if (!queue.length) $('#tray').hidden = true;
  }, 2500);
}

async function run(job, replace = false) {
  try {
    if (!job.id) {
      const res = await api('POST', '/api/uploads', { path: job.path, size: job.file.size, replace });
      job.id = res.id;
      if (res.done) return finish(job);
    }
    let tries = 0;
    while (job.sent < job.file.size) {
      if (job.state === 'cancelled') return;
      try {
        const res = await sendChunk(job, job.file.slice(job.sent, Math.min(job.sent + CHUNK, job.file.size)));
        job.sent = res.offset;
        tries = 0;
        if (res.done) return finish(job);
      } catch (err) {
        if (job.state === 'cancelled') return;
        if (err instanceof ApiError && err.status !== 409 && err.status < 500) throw err;
        if (++tries > 6) throw err;
        setStatus(job, `Connection hiccup, retrying… (${tries})`);
        await new Promise((r) => setTimeout(r, 1000 * 2 ** Math.min(tries, 5)));
        job.sent = await currentOffset(job);
      }
    }
  } catch (err) {
    fail(job, err);
  }
}

async function currentOffset(job) {
  const res = await fetch(`/api/uploads/${job.id}`, { method: 'HEAD', headers: { 'X-Fruit': '1' } });
  if (!res.ok) throw new ApiError(res.status, 'upload expired');
  return Number(res.headers.get('Upload-Offset'));
}

// XHR rather than fetch, because only XHR reports upload progress.
function sendChunk(job, blob) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    job.xhr = xhr;
    xhr.open('PATCH', `/api/uploads/${job.id}`);
    xhr.setRequestHeader('X-Fruit', '1');
    xhr.setRequestHeader('Upload-Offset', String(job.sent));
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = (e) => progress(job, job.sent + e.loaded);
    xhr.onload = () => {
      let body = {};
      try { body = JSON.parse(xhr.responseText); } catch {}
      if (xhr.status === 200) resolve(body);
      else reject(new ApiError(xhr.status, body.error || xhr.statusText));
    };
    xhr.onerror = () => reject(new Error('network'));
    xhr.onabort = () => reject(new Error('aborted'));
    xhr.send(blob);
  });
}

function finish(job) {
  job.state = 'done';
  progress(job, job.file.size);
  job.row.classList.add('done');
  setStatus(job, 'Done');
}

function fail(job, err) {
  job.state = 'failed';
  job.row.classList.add('failed');
  setStatus(job, err.message || 'Failed');
  const actions = job.row.querySelector('.row-actions');
  actions.replaceChildren();
  if (err instanceof ApiError && err.status === 409 && !job.id) {
    actions.append(h('button', { class: 'btn small', onclick: () => retry(job, true) }, 'Replace'));
  } else {
    actions.append(h('button', { class: 'btn small', onclick: () => retry(job, false) }, 'Retry'));
  }
  actions.append(h('button', { class: 'btn small ghost', onclick: () => cancel(job) }, 'Dismiss'));
}

function retry(job, replace) {
  job.row.classList.remove('failed');
  job.row.querySelector('.row-actions').replaceChildren(cancelButton(job));
  job.state = 'sending';
  running++;
  run(job, replace).finally(() => { running--; pump(); maybeFinished(); });
}

function cancel(job) {
  const wasActive = job.state === 'sending';
  job.state = 'cancelled';
  job.xhr?.abort();
  if (job.id) api('DELETE', `/api/uploads/${job.id}`).catch(() => {});
  job.row.remove();
  queue.splice(queue.indexOf(job), 1);
  if (!queue.length) $('#tray').hidden = true;
  if (!wasActive) maybeFinished();
}

const cancelButton = (job) => h('button', { class: 'icon-btn small', title: 'Cancel', 'aria-label': 'Cancel upload', onclick: () => cancel(job) }, '✕');

function renderRow(job) {
  const bar = h('div', { class: 'peel' }, h('div', { class: 'peel-fill' }), h('img', { src: 'svg/banana.svg', alt: '', class: 'peel-banana' }));
  const row = h('div', { class: 'tray-row' },
    h('div', { class: 'tray-name', title: job.path }, job.path.split('/').pop()),
    h('div', { class: 'tray-status muted small' }, `Waiting · ${fmtSize(job.file.size)}`),
    bar,
    h('div', { class: 'row-actions' }, cancelButton(job)));
  $('#tray').append(row);
  return row;
}

function progress(job, bytes) {
  const pct = job.file.size ? Math.min(100, (bytes / job.file.size) * 100) : 100;
  job.row.querySelector('.peel').style.setProperty('--p', `${pct}%`);
  if (job.state === 'sending') setStatus(job, `${fmtSize(bytes)} of ${fmtSize(job.file.size)}`);
}

function setStatus(job, text) {
  job.row.querySelector('.tray-status').textContent = text;
}

// --- reading dropped folders ------------------------------------------------

// filesFromDrop walks dropped folders so their structure is kept.
export async function filesFromDrop(dataTransfer) {
  const entries = [...dataTransfer.items].map((i) => i.webkitGetAsEntry?.()).filter(Boolean);
  if (!entries.length) return [...dataTransfer.files].map((file) => ({ file, rel: file.name }));
  const out = [];
  async function walk(entry, prefix) {
    if (entry.isFile) {
      const file = await new Promise((res, rej) => entry.file(res, rej));
      out.push({ file, rel: prefix + file.name });
    } else if (entry.isDirectory) {
      const reader = entry.createReader();
      let batch;
      do {
        batch = await new Promise((res, rej) => reader.readEntries(res, rej));
        for (const e of batch) await walk(e, `${prefix}${entry.name}/`);
      } while (batch.length);
    }
  }
  for (const e of entries) await walk(e, '');
  return out;
}
