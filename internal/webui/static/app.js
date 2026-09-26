// HDTVheadend dashboard — vanilla JS, no build step.

const state = { streams: [], settings: null, liveByID: {} };

// ---------- fetch helper ----------
async function api(path, opts) {
  const res = await fetch(path, Object.assign({ credentials: 'same-origin' }, opts));
  if (res.status === 401) {
    showLogin();
    throw new Error('unauthorized');
  }
  if (!res.ok) {
    const msg = await res.text().catch(() => res.statusText);
    throw new Error(msg || res.statusText);
  }
  const ct = res.headers.get('content-type') || '';
  return ct.includes('application/json') ? res.json() : null;
}

function toast(msg, isErr) {
  const el = document.createElement('div');
  el.className = 'toast' + (isErr ? ' err' : '');
  el.textContent = msg;
  document.getElementById('toasts').appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

// ---------- screens ----------
function showLogin() {
  document.getElementById('loginScreen').classList.remove('hidden');
  document.getElementById('app').classList.add('hidden');
  disconnectEvents();
  disconnectLogEvents();
}
function showApp() {
  document.getElementById('loginScreen').classList.add('hidden');
  document.getElementById('app').classList.remove('hidden');
  navigate('dashboard');
  connectEvents();
}

// ---------- real-time updates (Server-Sent Events) ----------
// One persistent connection to /api/events, live for as long as the
// dashboard is open. EventSource reconnects on its own if the connection
// drops (e.g. server restart), so there's no manual retry loop here.
let eventSource = null;

function connectEvents() {
  if (eventSource) return;
  eventSource = new EventSource('/api/events');
  eventSource.onmessage = (e) => {
    let status;
    try { status = JSON.parse(e.data); } catch { return; }
    onStatusEvent(status);
  };
  eventSource.addEventListener('open', () => setLiveIndicator(true));
  eventSource.onerror = () => setLiveIndicator(false);
}

function disconnectEvents() {
  if (eventSource) { eventSource.close(); eventSource = null; }
  setLiveIndicator(false);
}

function setLiveIndicator(connected) {
  const el = document.getElementById('liveIndicator');
  const text = document.getElementById('liveIndicatorText');
  if (el) el.classList.toggle('connected', connected);
  if (text) text.textContent = connected ? 'Live' : 'Reconnecting…';
}

// onStatusEvent is the single entry point for a fresh status payload,
// whether it arrived via SSE or a one-off fetch (loadDashboard/loadTuners/
// loadStreams's initial load). It updates the live-data cache and
// re-renders whichever view is currently on screen.
function onStatusEvent(status) {
  state.liveByID = {};
  (status.streams || []).forEach(s => { state.liveByID[s.id] = s; });

  if (currentView === 'dashboard') renderDashboard(status);
  else if (currentView === 'tuners') renderTunersView(status);
  else if (currentView === 'streams') renderStreams();
}

// ---------- theme ----------
(function initTheme() {
  let saved = null;
  try { saved = localStorage.getItem('nova_theme'); } catch {}
  if (saved) document.documentElement.setAttribute('data-theme', saved);
})();
document.getElementById('themeToggle').addEventListener('click', () => {
  const cur = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
  const next = cur === 'light' ? 'dark' : 'light';
  document.documentElement.setAttribute('data-theme', next);
  try { localStorage.setItem('nova_theme', next); } catch {}
});

// ---------- login ----------
document.getElementById('loginForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const username = document.getElementById('loginUser').value;
  const password = document.getElementById('loginPass').value;
  const errEl = document.getElementById('loginErr');
  errEl.textContent = '';
  try {
    const res = await fetch('/api/login', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ Username: username, Password: password }),
    });
    if (res.ok) { showApp(); return; }
    errEl.textContent = res.status === 429 ? 'Too many attempts — wait a moment.' : 'Invalid credentials.';
  } catch {
    errEl.textContent = 'Could not reach server.';
  }
});

async function logout() {
  await api('/api/logout', { method: 'POST' }).catch(() => {});
  showLogin();
}

// ---------- nav ----------
let currentView = 'dashboard';

function navigate(view) {
  currentView = view;
  document.querySelectorAll('.nav a[data-view]').forEach(a => a.classList.toggle('active', a.dataset.view === view));
  document.querySelectorAll('.view').forEach(v => v.classList.toggle('hidden', v.id !== 'view-' + view));
  if (view === 'dashboard') loadDashboard();
  if (view === 'tuners') loadTuners();
  if (view === 'streams') loadStreams();
  if (view === 'settings') loadSettings();
  if (view === 'logs') loadLogs();
  if (view !== 'logs') disconnectLogEvents();
}
document.querySelectorAll('.nav a[data-view]').forEach(a => a.addEventListener('click', () => navigate(a.dataset.view)));

function formatUptime(sec) {
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}
function formatBps(bps) {
  const mbit = (bps * 8) / 1e6;
  return mbit >= 1 ? `${mbit.toFixed(1)} Mbit` : `${(bps / 1000).toFixed(1)} kB/s`;
}

// ---------- dashboard ----------
// loadDashboard/loadTuners fetch the stream config (which rarely changes)
// once per navigation and do a one-off status fetch for a fast first
// paint; after that, live numbers arrive via the /api/events SSE stream
// (see connectEvents/onStatusEvent above) and are rendered by
// renderDashboard/renderTunersView directly, with no further polling.
async function loadDashboard() {
  const [status, streams] = await Promise.all([api('/api/status'), api('/api/streams')]);
  state.streams = streams || [];
  renderDashboard(status);
}

function renderDashboard(status) {
  document.getElementById('dStreamCount').textContent = status.stream_count;
  document.getElementById('dEnabledCount').textContent = status.enabled_count;
  document.getElementById('dTunerCount').textContent = status.tuner_count;
  document.getElementById('dClientCount').textContent = status.client_count;
  document.getElementById('dInputBps').textContent = formatBps(status.input_bps);
  document.getElementById('dOutputBps').textContent = formatBps(status.output_bps);

  const dvbRunning = (status.streams || []).filter(s => s.running && isDVB(s.id)).length;
  const dvbTotal = status.tuner_count;
  document.getElementById('dTunerSub').textContent = `${dvbTotal} configured`;
  document.getElementById('dTunerMeta').textContent = `${dvbRunning} / ${dvbTotal} running`;

  renderTunerTable(document.getElementById('dashboardTunerTableWrap'), status.streams || []);

  function isDVB(id) {
    const s = state.streams.find(x => x.id === id);
    return s && s.input && s.input.type === 'dvb';
  }
}

async function loadTuners() {
  const [status, streams] = await Promise.all([api('/api/status'), api('/api/streams')]);
  state.streams = streams || [];
  renderTunersView(status);
}

function renderTunersView(status) {
  const dvbRunning = (status.streams || []).filter(s => {
    const cfg = state.streams.find(x => x.id === s.id);
    return s.running && cfg && cfg.input && cfg.input.type === 'dvb';
  }).length;
  document.getElementById('tTunerMeta').textContent = `${dvbRunning} / ${status.tuner_count} running`;
  renderTunerTable(document.getElementById('tunerTableWrap'), status.streams || []);
}

function renderTunerTable(wrap, statusStreams) {
  const statByID = {};
  (statusStreams || []).forEach(s => { statByID[s.id] = s; });

  const tuners = state.streams.filter(s => s.input && s.input.type === 'dvb');
  if (!tuners.length) {
    wrap.innerHTML = `<div style="padding:24px 20px;"><span class="empty-note">No DVB tuners configured yet. Add a stream with input type "DVB tuner" to see it here.</span></div>`;
    return;
  }

  const rows = tuners.map(s => {
    const i = s.input;
    const live = statByID[s.id];
    const running = live ? live.running : false;
    const freqLine = i.frequency_khz ? `${i.frequency_khz} ${i.polarization || ''}`.trim() : '—';
    const detailBits = [];
    if (i.symbol_rate_ks) detailBits.push(`${i.symbol_rate_ks} kS/s`);
    if (i.bandwidth_hz) detailBits.push(`${(i.bandwidth_hz / 1e6).toFixed(1)} MHz`);
    if (i.dvb_system) detailBits.push(i.dvb_system);
    const detail = detailBits.join(' · ') || (i.modulation || '');

    return `
      <tr>
        <td><span class="primary">${i.dvb_adapter ?? 0}:${i.dvb_frontend ?? 0}</span></td>
        <td>
          <div class="primary">${escapeHtml(s.name || s.id)}</div>
          <div class="secondary">${escapeHtml(detail)}</div>
        </td>
        <td>
          <div>${escapeHtml(freqLine)}</div>
          ${live ? `<div class="secondary">${live.clients} client${live.clients === 1 ? '' : 's'} · in ${formatBps(live.input_bps)} · out ${formatBps(live.output_bps)}</div>` : ''}
        </td>
        <td><span class="pill ${running ? 'good' : 'dim'}">${running ? 'Running' : 'Stopped'}</span></td>
        <td>
          <div class="meter unmonitored">
            <span class="m-label">SIG</span><div class="m-track"></div>
          </div>
          <div class="meter unmonitored">
            <span class="m-label">QUA</span><div class="m-track"></div>
          </div>
          <div class="secondary" style="margin-top:2px;">not monitored yet</div>
        </td>
      </tr>`;
  }).join('');

  wrap.innerHTML = `
    <table class="data">
      <thead><tr><th>Adapter</th><th>Name</th><th>Frequency</th><th>Status</th><th>Signal / Quality</th></tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
}

// ---------- streams ----------
async function loadStreams() {
  const [streams, status] = await Promise.all([api('/api/streams'), api('/api/status')]);
  state.streams = streams || [];
  state.liveByID = {};
  (status.streams || []).forEach(s => { state.liveByID[s.id] = s; });
  renderStreams();
}

// refreshStatus re-polls whatever live data the current view shows, without
// a full navigation — called right after an action (toggle/delete/save) so
// the dashboard/streams/tuners view reflects it immediately rather than
// waiting for the next 10s poll.
function refreshStatus() {
  if (currentView === 'dashboard') loadDashboard();
  else if (currentView === 'tuners') loadTuners();
  else if (currentView === 'streams') loadStreams();
}

function renderStreams() {
  const grid = document.getElementById('streamGrid');
  const empty = document.getElementById('streamEmpty');
  grid.innerHTML = '';
  if (!state.streams.length) {
    empty.classList.remove('hidden');
    return;
  }
  empty.classList.add('hidden');

  for (const s of state.streams) {
    const card = document.createElement('div');
    card.className = 'card';

    const initials = (s.name || s.id || '?').slice(0, 2).toUpperCase();
    const logo = s.logo_url
      ? `<img src="${escapeAttr(s.logo_url)}" alt="">`
      : initials;

    const outBadges = (s.outputs || []).map(o => `<span class="badge">${o.type}</span>`).join('');
    const caBadge = s.ca && s.ca.type ? `<span class="badge ca">${s.ca.type}</span>` : '';
    const inBadge = `<span class="badge">${s.input ? s.input.type : '—'}</span>`;
    const modeBadge = s.mode === 'on_demand' ? `<span class="badge">on demand</span>` : '';

    const live = state.liveByID[s.id];
    let liveLine = '';
    if (s.enabled && live) {
      const stateLabel = !live.running
        ? `<span class="pill dim">Stopped</span>`
        : live.on_demand && !live.input_active
          ? `<span class="pill dim">Idle (on demand)</span>`
          : `<span class="pill good">Live</span>`;
      liveLine = `<div class="secondary" style="margin-top:8px;">${stateLabel} &nbsp; in ${formatBps(live.input_bps)} · out ${formatBps(live.output_bps)} · ${live.clients} client${live.clients === 1 ? '' : 's'}</div>`;
    }

    card.innerHTML = `
      <div class="card-head">
        <div class="logo">${logo}</div>
        <div class="card-title">
          <div class="name">${escapeHtml(s.name || s.id)}</div>
          <div class="group">${escapeHtml(s.group || 'Ungrouped')}</div>
        </div>
        <label class="switch">
          <input type="checkbox" ${s.enabled ? 'checked' : ''} data-id="${escapeAttr(s.id)}" class="enable-toggle">
          <span class="track"></span>
        </label>
      </div>
      <div class="badges">${inBadge}${caBadge}${outBadges}${modeBadge}</div>
      ${liveLine}
      <div class="card-actions">
        <button class="btn secondary status-btn" data-id="${escapeAttr(s.id)}">Status</button>
        <button class="btn secondary edit-btn" data-id="${escapeAttr(s.id)}">Edit</button>
        <button class="btn danger delete-btn" data-id="${escapeAttr(s.id)}">Delete</button>
      </div>
    `;
    grid.appendChild(card);
  }

  grid.querySelectorAll('.enable-toggle').forEach(el => el.addEventListener('change', onToggleEnabled));
  grid.querySelectorAll('.status-btn').forEach(el => el.addEventListener('click', () => openStatusModal(el.dataset.id)));
  grid.querySelectorAll('.edit-btn').forEach(el => el.addEventListener('click', () => openStreamModal(el.dataset.id)));
  grid.querySelectorAll('.delete-btn').forEach(el => el.addEventListener('click', () => deleteStream(el.dataset.id)));
}

async function onToggleEnabled(e) {
  const id = e.target.dataset.id;
  const s = state.streams.find(x => x.id === id);
  if (!s) return;
  s.enabled = e.target.checked;
  try {
    await api('/api/streams/' + encodeURIComponent(id), {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(s),
    });
    toast(`${s.name || s.id} ${s.enabled ? 'enabled' : 'disabled'}`);
    refreshStatus();
  } catch (err) {
    e.target.checked = !s.enabled;
    toast('Failed to update stream: ' + err.message, true);
  }
}

async function deleteStream(id) {
  const s = state.streams.find(x => x.id === id);
  if (!confirm(`Remove "${s ? (s.name || s.id) : id}"? This stops the stream immediately.`)) return;
  try {
    await api('/api/streams/' + encodeURIComponent(id), { method: 'DELETE' });
    toast('Stream removed');
    loadStreams();
    refreshStatus();
  } catch (err) {
    toast('Failed to remove stream: ' + err.message, true);
  }
}

// ---------- add/edit modal ----------
const INPUT_FIELDS = {
  httpts: ['url'], udp: ['addr', 'iface'], rtp: ['addr', 'iface'],
  hls: ['url'], dash: ['url'],
  dvb: ['dvb_adapter', 'dvb_frontend', 'dvb_system', 'frequency_khz', 'symbol_rate_ks', 'polarization', 'bandwidth_hz', 'modulation'],
  srt: ['addr', 'srt_mode', 'srt_passphrase', 'srt_stream_id', 'srt_latency_ms'],
  rtsp: ['url'],
  rtmp: ['rtmp_listen_ip', 'rtmp_listen_port', 'rtmp_app', 'rtmp_stream_key', 'rtmp_note'],
  rtmps: ['rtmp_listen_ip', 'rtmp_listen_port', 'rtmp_app', 'rtmp_stream_key', 'tls_cert_file', 'tls_key_file', 'rtmp_note'],
  rtmp_pull: ['url', 'rtmp_pull_note'],
  youtube: ['url', 'youtube_note'],
  webrtc: ['path', 'webrtc_note'],
};
const NUMERIC_FIELDS = ['dvb_adapter', 'dvb_frontend', 'frequency_khz', 'symbol_rate_ks', 'bandwidth_hz', 'srt_latency_ms'];

function openStreamModal(id) {
  const editing = id ? state.streams.find(s => s.id === id) : null;
  const s = editing || { id: '', name: '', group: '', logo_url: '', epg_id: '', enabled: true, input: { type: 'httpts' }, ca: {}, outputs: [] };

  const backdrop = document.getElementById('modalBackdrop');
  backdrop.innerHTML = renderStreamForm(s, !!editing);
  backdrop.classList.remove('hidden');

  const form = document.getElementById('streamForm');
  const inputType = document.getElementById('f_input_type');
  const caType = document.getElementById('f_ca_type');

  updateInputFields(inputType.value);
  updateCAFields(caType.value);
  inputType.addEventListener('change', () => updateInputFields(inputType.value));
  caType.addEventListener('change', () => updateCAFields(caType.value));
  populateRTMPListenIPs();

  ['httpts', 'udp', 'srt', 'rtsp', 'hls', 'dash', 'rtmp', 'webrtc'].forEach(kind => {
    const cb = document.getElementById('f_out_' + kind);
    cb.addEventListener('change', () => toggleSection('f_out_' + kind + '_fields', cb.checked));
  });

  document.getElementById('modalCancel').addEventListener('click', closeModal);
  // Deliberately no backdrop-click-to-close: an accidental click outside
  // the modal shouldn't discard an in-progress edit. Cancel or Escape only.
  document.addEventListener('keydown', escToClose);
  form.addEventListener('submit', (e) => { e.preventDefault(); saveStreamForm(!!editing, s.id); });
}

function closeModal() {
  const backdrop = document.getElementById('modalBackdrop');
  backdrop.classList.add('hidden');
  backdrop.innerHTML = '';
  document.removeEventListener('keydown', escToClose);
}

function escToClose(e) {
  if (e.key === 'Escape') closeModal();
}

function toggleSection(id, show) {
  document.getElementById(id).classList.toggle('hidden', !show);
}

function updateInputFields(type) {
  document.querySelectorAll('.input-field-group').forEach(el => el.classList.add('hidden'));
  const active = INPUT_FIELDS[type] || [];
  active.forEach(f => {
    const el = document.getElementById('grp_' + f);
    if (el) el.classList.remove('hidden');
  });
}

// populateRTMPListenIPs fills the RTMP/RTMPS "Listen IP" dropdown with this
// machine's own local addresses (fetched from the server — a browser can't
// enumerate the server's network interfaces itself), so picking one avoids
// ever configuring an address this machine doesn't actually own.
async function populateRTMPListenIPs() {
  const sel = document.getElementById('f_rtmp_listen_ip');
  if (!sel) return;
  const current = sel.dataset.current || '0.0.0.0';
  try {
    const resp = await api('/api/network-interfaces');
    const addrs = (resp.addrs && resp.addrs.length) ? resp.addrs.slice() : ['0.0.0.0'];
    if (!addrs.includes(current)) addrs.push(current); // preserve a configured IP even if not currently present on this machine
    sel.innerHTML = addrs.map(a =>
      `<option value="${escapeAttr(a)}" ${a === current ? 'selected' : ''}>${a === '0.0.0.0' ? '0.0.0.0 (all interfaces)' : escapeHtml(a)}</option>`
    ).join('');
  } catch (err) {
    // Leave just the default 0.0.0.0 option; not fatal.
  }
}

function updateCAFields(type) {
  toggleSection('f_ca_key_group', type === 'biss_sw' || type === 'biss_cw');
  document.getElementById('f_ca_key_label').textContent =
    type === 'biss_sw' ? 'Session word (12 hex digits)' : 'Control word (16 hex digits)';
}

function val(id) { const el = document.getElementById(id); return el ? el.value.trim() : ''; }
function num(id) { const v = val(id); return v === '' ? undefined : Number(v); }

function saveStreamForm(editing, originalId) {
  const inputType = val('f_input_type');
  const input = { type: inputType };
  const PSEUDO_FIELDS = ['rtmp_listen_ip', 'rtmp_listen_port']; // composed into addr below, not real backend fields
  (INPUT_FIELDS[inputType] || []).forEach(f => {
    if (PSEUDO_FIELDS.includes(f)) return;
    const raw = val('f_' + f);
    if (raw === '') return;
    // Field names here (f) are already the exact snake_case JSON keys the
    // Go backend expects (see config.Input's `json:"..."` tags) — do not
    // camelCase them, or the backend silently drops the field.
    input[f] = NUMERIC_FIELDS.includes(f) ? Number(raw) : raw;
  });
  if (inputType === 'rtmp' || inputType === 'rtmps') {
    const ip = val('f_rtmp_listen_ip') || '0.0.0.0';
    const port = val('f_rtmp_listen_port') || '1935';
    input.addr = `${ip}:${port}`;
  }

  const caType = val('f_ca_type');
  const ca = caType ? { type: caType, key: val('f_ca_key') } : {};

  const outputs = [];
  const id = val('f_id') || slugify(val('f_name'));
  if (document.getElementById('f_out_httpts').checked) {
    outputs.push({ type: 'httpts', path: val('f_out_httpts_path') || `/stream/${id}.ts` });
  }
  if (document.getElementById('f_out_udp').checked) {
    const udpIP = val('f_out_udp_ip');
    const udpPort = val('f_out_udp_port') || '5000';
    outputs.push({
      type: 'udp',
      addr: `${udpIP}:${udpPort}`,
      ttl: num('f_out_udp_ttl') || 0,
      rtp: document.getElementById('f_out_udp_rtp').checked,
    });
  }
  if (document.getElementById('f_out_srt').checked) {
    outputs.push({
      type: 'srt',
      addr: val('f_out_srt_addr'),
      srt_mode: val('f_out_srt_mode'),
      srt_passphrase: val('f_out_srt_passphrase'),
      srt_stream_id: val('f_out_srt_stream_id'),
      srt_latency_ms: num('f_out_srt_latency_ms') || 0,
    });
  }
  if (document.getElementById('f_out_rtsp').checked) {
    outputs.push({ type: 'rtsp', addr: val('f_out_rtsp_addr') });
  }
  if (document.getElementById('f_out_hls').checked) {
    outputs.push({
      type: 'hls',
      path: val('f_out_hls_path') || `/hls/${id}/`,
      segment_seconds: num('f_out_hls_seg_seconds') || 0,
      segment_count: num('f_out_hls_seg_count') || 0,
    });
  }
  if (document.getElementById('f_out_dash').checked) {
    outputs.push({
      type: 'dash',
      path: val('f_out_dash_path') || `/dash/${id}/`,
      segment_seconds: num('f_out_dash_seg_seconds') || 0,
      segment_count: num('f_out_dash_seg_count') || 0,
    });
  }
  if (document.getElementById('f_out_rtmp').checked) {
    outputs.push({ type: 'rtmp', url: val('f_out_rtmp_url') });
  }
  if (document.getElementById('f_out_webrtc').checked) {
    outputs.push({ type: 'webrtc', path: val('f_out_webrtc_path') || `/whep/${id}` });
  }

  const stream = {
    id, name: val('f_name') || id, group: val('f_group'),
    logo_url: val('f_logo_url'), epg_id: val('f_epg_id'),
    enabled: document.getElementById('f_enabled').checked,
    mode: val('f_mode') || 'static',
    input, ca, outputs,
  };

  const req = editing
    ? api('/api/streams/' + encodeURIComponent(originalId), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(stream) })
    : api('/api/streams', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(stream) });

  req.then(() => {
    toast(editing ? 'Stream updated' : 'Stream added');
    closeModal();
    loadStreams();
    refreshStatus();
  }).catch(err => toast('Save failed: ' + err.message, true));
}

function slugify(s) {
  return (s || 'stream').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/(^-|-$)/g, '') || ('stream-' + Date.now());
}
function escapeHtml(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function escapeAttr(s) { return escapeHtml(s); }

function renderStreamForm(s, editing) {
  const i = s.input || {};
  const ca = s.ca || {};
  const hasHTTPOut = (s.outputs || []).find(o => o.type === 'httpts');
  const hasUDPOut = (s.outputs || []).find(o => o.type === 'udp');
  const hasSRTOut = (s.outputs || []).find(o => o.type === 'srt');
  const hasRTSPOut = (s.outputs || []).find(o => o.type === 'rtsp');
  const hasHLSOut = (s.outputs || []).find(o => o.type === 'hls');
  const hasDASHOut = (s.outputs || []).find(o => o.type === 'dash');
  const hasRTMPOut = (s.outputs || []).find(o => o.type === 'rtmp');
  const hasWebRTCOut = (s.outputs || []).find(o => o.type === 'webrtc');
  const [rtmpListenIP, rtmpListenPort] = i.addr ? splitHostPort(i.addr) : ['0.0.0.0', ''];
  const [udpOutIP, udpOutPort] = hasUDPOut && hasUDPOut.addr ? splitHostPort(hasUDPOut.addr) : ['', ''];

  return `
  <div class="modal">
    <h3>${editing ? 'Edit stream' : 'Add stream'}</h3>
    <form id="streamForm">
      <input type="hidden" id="f_id" value="${escapeAttr(s.id)}">
      <div class="row">
        <div class="field"><label>Name</label><input type="text" id="f_name" value="${escapeAttr(s.name)}" required></div>
        <div class="field"><label>Group</label><input type="text" id="f_group" value="${escapeAttr(s.group)}" placeholder="Sports"></div>
      </div>
      <div class="row">
        <div class="field"><label>Logo URL</label><input type="text" id="f_logo_url" value="${escapeAttr(s.logo_url)}"></div>
        <div class="field"><label>EPG ID</label><input type="text" id="f_epg_id" value="${escapeAttr(s.epg_id)}"></div>
      </div>

      <div class="section-label">Input</div>
      <div class="field">
        <label>Type</label>
        <select id="f_input_type">
          <option value="httpts" ${i.type === 'httpts' ? 'selected' : ''}>HTTP-TS</option>
          <option value="udp" ${i.type === 'udp' ? 'selected' : ''}>UDP</option>
          <option value="rtp" ${i.type === 'rtp' ? 'selected' : ''}>RTP</option>
          <option value="hls" ${i.type === 'hls' ? 'selected' : ''}>HLS (m3u8)</option>
          <option value="dash" ${i.type === 'dash' ? 'selected' : ''}>DASH (MPD)</option>
          <option value="dvb" ${i.type === 'dvb' ? 'selected' : ''}>DVB tuner</option>
          <option value="srt" ${i.type === 'srt' ? 'selected' : ''}>SRT</option>
          <option value="rtsp" ${i.type === 'rtsp' ? 'selected' : ''}>RTSP</option>
          <option value="rtmp" ${i.type === 'rtmp' ? 'selected' : ''}>RTMP server (accept a push — experimental, see hint below)</option>
          <option value="rtmps" ${i.type === 'rtmps' ? 'selected' : ''}>RTMPS server (accept a push — experimental, see hint below)</option>
          <option value="rtmp_pull" ${i.type === 'rtmp_pull' ? 'selected' : ''}>RTMP pull (connect to an existing remote stream)</option>
          <option value="youtube" ${i.type === 'youtube' ? 'selected' : ''}>YouTube (paste your own extracted URL)</option>
          <option value="webrtc" ${i.type === 'webrtc' ? 'selected' : ''}>WebRTC (WHIP publish — video only)</option>
        </select>
      </div>
      <div id="grp_url" class="field input-field-group">
        <label>Source URL</label><input type="text" id="f_url" value="${escapeAttr(i.url)}" placeholder="http://.../stream.ts">
      </div>
      <div id="grp_addr" class="field input-field-group">
        <label>Listen address</label><input type="text" id="f_addr" value="${escapeAttr(i.addr)}" placeholder="239.1.1.1:5000">
      </div>
      <div id="grp_iface" class="field input-field-group">
        <label>Multicast interface (optional)</label><input type="text" id="f_iface" value="${escapeAttr(i.iface)}" placeholder="eth0">
      </div>
      <div id="grp_dvb_adapter" class="field input-field-group">
        <label>DVB adapter #</label><input type="number" id="f_dvb_adapter" value="${i.dvb_adapter ?? 0}">
      </div>
      <div id="grp_dvb_frontend" class="field input-field-group">
        <label>Frontend #</label><input type="number" id="f_dvb_frontend" value="${i.dvb_frontend ?? 0}">
      </div>
      <div id="grp_dvb_system" class="field input-field-group">
        <label>Delivery system</label>
        <select id="f_dvb_system">
          ${['DVBS','DVBS2','DVBT','DVBT2','DVBC'].map(sys => `<option ${i.dvb_system === sys ? 'selected' : ''}>${sys}</option>`).join('')}
        </select>
      </div>
      <div id="grp_frequency_khz" class="field input-field-group">
        <label>Frequency (kHz)</label><input type="number" id="f_frequency_khz" value="${i.frequency_khz ?? ''}">
      </div>
      <div id="grp_symbol_rate_ks" class="field input-field-group">
        <label>Symbol rate (kS/s)</label><input type="number" id="f_symbol_rate_ks" value="${i.symbol_rate_ks ?? ''}">
      </div>
      <div id="grp_polarization" class="field input-field-group">
        <label>Polarization</label>
        <select id="f_polarization">${['H','V','L','R'].map(p => `<option ${i.polarization === p ? 'selected' : ''}>${p}</option>`).join('')}</select>
      </div>
      <div id="grp_bandwidth_hz" class="field input-field-group">
        <label>Bandwidth (Hz)</label><input type="number" id="f_bandwidth_hz" value="${i.bandwidth_hz ?? ''}">
      </div>
      <div id="grp_modulation" class="field input-field-group">
        <label>Modulation</label><input type="text" id="f_modulation" value="${escapeAttr(i.modulation)}" placeholder="AUTO">
      </div>
      <div id="grp_srt_mode" class="field input-field-group">
        <label>SRT mode</label>
        <select id="f_srt_mode">
          <option value="listener" ${i.srt_mode === 'listener' ? 'selected' : ''}>Listener (accept incoming)</option>
          <option value="caller" ${i.srt_mode === 'caller' ? 'selected' : ''}>Caller (dial out)</option>
        </select>
      </div>
      <div id="grp_srt_passphrase" class="field input-field-group">
        <label>Passphrase (optional, 10-80 chars, enables encryption)</label>
        <input type="text" id="f_srt_passphrase" value="${escapeAttr(i.srt_passphrase)}">
      </div>
      <div id="grp_srt_stream_id" class="field input-field-group">
        <label>Stream ID (optional)</label>
        <input type="text" id="f_srt_stream_id" value="${escapeAttr(i.srt_stream_id)}">
      </div>
      <div id="grp_srt_latency_ms" class="field input-field-group">
        <label>Latency (ms, 0 = library default)</label>
        <input type="number" id="f_srt_latency_ms" value="${i.srt_latency_ms ?? ''}">
      </div>
      <div id="grp_rtmp_listen_ip" class="field input-field-group">
        <label>Listen IP</label>
        <select id="f_rtmp_listen_ip" data-current="${escapeAttr(rtmpListenIP || '0.0.0.0')}">
          <option value="0.0.0.0">0.0.0.0 (all interfaces)</option>
        </select>
      </div>
      <div id="grp_rtmp_listen_port" class="field input-field-group">
        <label>Listen port</label>
        <input type="number" id="f_rtmp_listen_port" value="${rtmpListenPort || 1935}" placeholder="1935">
      </div>
      <div id="grp_rtmp_app" class="field input-field-group">
        <label>App path (cosmetic — only changes the example ingest URL shown under Status; a publisher's own app name isn't checked)</label>
        <input type="text" id="f_rtmp_app" value="${escapeAttr(i.rtmp_app)}" placeholder="live">
      </div>
      <div id="grp_rtmp_stream_key" class="field input-field-group">
        <label>Stream key (optional; empty = accept any)</label>
        <input type="text" id="f_rtmp_stream_key" value="${escapeAttr(i.rtmp_stream_key)}">
      </div>
      <div id="grp_tls_cert_file" class="field input-field-group">
        <label>TLS certificate file (optional; empty = auto-generate self-signed)</label>
        <input type="text" id="f_tls_cert_file" value="${escapeAttr(i.tls_cert_file)}" placeholder="/etc/hdtvheadend/cert.pem">
      </div>
      <div id="grp_tls_key_file" class="field input-field-group">
        <label>TLS private key file (optional; empty = auto-generate self-signed)</label>
        <input type="text" id="f_tls_key_file" value="${escapeAttr(i.tls_key_file)}" placeholder="/etc/hdtvheadend/key.pem">
      </div>
      <div id="grp_path" class="field input-field-group">
        <label>WHIP publish path</label>
        <input type="text" id="f_path" value="${escapeAttr(i.path)}" placeholder="/whip/mystream">
      </div>
      <div id="grp_rtmp_note" class="input-field-group empty-note" style="margin:-4px 0 12px;">RTMP/RTMPS input is experimental: ffmpeg's decoder fails on the resulting stream, but the same symptom shows up on unrelated real broadcast content too — likely an ffmpeg-specific quirk, not a confirmed defect. Real player compatibility isn't verified either way yet. RTMP/RTMPS output (pushing to YouTube/Twitch etc.) is unaffected. With no certificate configured, RTMPS auto-generates a self-signed one — publishers must disable certificate verification, same as pointing OBS at any other self-signed target.</div>
      <div id="grp_youtube_note" class="input-field-group empty-note" style="margin:-4px 0 12px;">HDTVheadend does not scrape YouTube or bypass its bot detection. Extract the .m3u8 URL yourself (e.g. from your browser's Network tab) for a live stream you own or are authorized to redistribute, and paste it above as the Source URL. VOD is not supported this way.</div>
      <div id="grp_webrtc_note" class="input-field-group empty-note" style="margin:-4px 0 12px;">WHIP publisher (OBS 30+, a browser, or any WHIP client) POSTs its SDP offer to this path on this server's normal HTTP port, e.g. http://host:8088/whip/mystream. Video (H.264) only — WebRTC's mandatory audio codec is Opus, which this binary's AAC-based pipeline can't carry; any audio track offered is negotiated but discarded.</div>
      <div id="grp_rtmp_pull_note" class="input-field-group empty-note" style="margin:-4px 0 12px;">Use this when a stream already exists somewhere else and you want to pull/play it (e.g. a VPS or another media server) — the opposite of "RTMP server" above, which instead waits for something to push to you. Enter the full URL as given, e.g. rtmp://195.250.31.2/static/TVM (use rtmps:// for TLS). The app/path segment is passed through but otherwise doesn't matter; only the last path segment (the stream name) is used to request the stream.</div>

      <div class="section-label">Conditional access</div>
      <div class="field">
        <label>Type</label>
        <select id="f_ca_type">
          <option value="">None</option>
          <option value="biss_sw" ${ca.type === 'biss_sw' ? 'selected' : ''}>BISS-1 session word</option>
          <option value="biss_cw" ${ca.type === 'biss_cw' ? 'selected' : ''}>BISS control word</option>
          <option value="ci_plus" ${ca.type === 'ci_plus' ? 'selected' : ''}>CI/CI+ hardware CAM</option>
        </select>
      </div>
      <div id="f_ca_key_group" class="field">
        <label id="f_ca_key_label">Key</label>
        <input type="text" id="f_ca_key" value="${escapeAttr(ca.key)}" placeholder="hex digits">
      </div>

      <div class="section-label">Outputs</div>
      <div class="checkline"><input type="checkbox" id="f_out_httpts" ${hasHTTPOut ? 'checked' : ''}> HTTP-TS</div>
      <div id="f_out_httpts_fields" class="sub-fields ${hasHTTPOut ? '' : 'hidden'}">
        <div class="field"><label>Path</label><input type="text" id="f_out_httpts_path" value="${escapeAttr(hasHTTPOut ? hasHTTPOut.path : '')}" placeholder="/stream/channel.ts"></div>
      </div>
      <div class="checkline"><input type="checkbox" id="f_out_udp" ${hasUDPOut ? 'checked' : ''}> UDP / multicast</div>
      <div id="f_out_udp_fields" class="sub-fields ${hasUDPOut ? '' : 'hidden'}">
        <div class="row">
          <div class="field"><label>Destination (IP only)</label><input type="text" id="f_out_udp_ip" value="${escapeAttr(udpOutIP)}" placeholder="239.1.1.1"></div>
          <div class="field"><label>Port</label><input type="number" id="f_out_udp_port" value="${escapeAttr(udpOutPort)}" placeholder="5000"></div>
          <div class="field"><label>TTL</label><input type="number" id="f_out_udp_ttl" value="${hasUDPOut ? (hasUDPOut.ttl || '') : ''}"></div>
        </div>
        <div class="checkline"><input type="checkbox" id="f_out_udp_rtp" ${hasUDPOut && hasUDPOut.rtp ? 'checked' : ''}> Wrap in RTP</div>
      </div>

      <div class="checkline"><input type="checkbox" id="f_out_srt" ${hasSRTOut ? 'checked' : ''}> SRT</div>
      <div id="f_out_srt_fields" class="sub-fields ${hasSRTOut ? '' : 'hidden'}">
        <div class="row">
          <div class="field"><label>Address</label><input type="text" id="f_out_srt_addr" value="${escapeAttr(hasSRTOut ? hasSRTOut.addr : '')}" placeholder="0.0.0.0:9000"></div>
          <div class="field">
            <label>Mode</label>
            <select id="f_out_srt_mode">
              <option value="listener" ${!hasSRTOut || hasSRTOut.srt_mode === 'listener' ? 'selected' : ''}>Listener (serve subscribers)</option>
              <option value="caller" ${hasSRTOut && hasSRTOut.srt_mode === 'caller' ? 'selected' : ''}>Caller (push out)</option>
            </select>
          </div>
        </div>
        <div class="row">
          <div class="field"><label>Passphrase (optional)</label><input type="text" id="f_out_srt_passphrase" value="${escapeAttr(hasSRTOut ? hasSRTOut.srt_passphrase : '')}"></div>
          <div class="field"><label>Stream ID (optional)</label><input type="text" id="f_out_srt_stream_id" value="${escapeAttr(hasSRTOut ? hasSRTOut.srt_stream_id : '')}"></div>
        </div>
        <div class="field"><label>Latency (ms, 0 = default)</label><input type="number" id="f_out_srt_latency_ms" value="${hasSRTOut ? (hasSRTOut.srt_latency_ms ?? '') : ''}"></div>
      </div>

      <div class="checkline"><input type="checkbox" id="f_out_rtsp" ${hasRTSPOut ? 'checked' : ''}> RTSP</div>
      <div id="f_out_rtsp_fields" class="sub-fields ${hasRTSPOut ? '' : 'hidden'}">
        <div class="field"><label>Listen address</label><input type="text" id="f_out_rtsp_addr" value="${escapeAttr(hasRTSPOut ? hasRTSPOut.addr : '')}" placeholder="0.0.0.0:8554"></div>
      </div>

      <div class="checkline"><input type="checkbox" id="f_out_hls" ${hasHLSOut ? 'checked' : ''}> HLS</div>
      <div id="f_out_hls_fields" class="sub-fields ${hasHLSOut ? '' : 'hidden'}">
        <div class="field"><label>Path</label><input type="text" id="f_out_hls_path" value="${escapeAttr(hasHLSOut ? hasHLSOut.path : '')}" placeholder="/hls/channel/"></div>
        <div class="row">
          <div class="field"><label>Segment seconds (default 4)</label><input type="number" id="f_out_hls_seg_seconds" value="${hasHLSOut ? (hasHLSOut.segment_seconds ?? '') : ''}"></div>
          <div class="field"><label>Segment count (default 6)</label><input type="number" id="f_out_hls_seg_count" value="${hasHLSOut ? (hasHLSOut.segment_count ?? '') : ''}"></div>
        </div>
      </div>

      <div class="checkline"><input type="checkbox" id="f_out_dash" ${hasDASHOut ? 'checked' : ''}> DASH</div>
      <div id="f_out_dash_fields" class="sub-fields ${hasDASHOut ? '' : 'hidden'}">
        <div class="field"><label>Path</label><input type="text" id="f_out_dash_path" value="${escapeAttr(hasDASHOut ? hasDASHOut.path : '')}" placeholder="/dash/channel/"></div>
        <div class="row">
          <div class="field"><label>Segment seconds (default 4)</label><input type="number" id="f_out_dash_seg_seconds" value="${hasDASHOut ? (hasDASHOut.segment_seconds ?? '') : ''}"></div>
          <div class="field"><label>Segment count (default 6)</label><input type="number" id="f_out_dash_seg_count" value="${hasDASHOut ? (hasDASHOut.segment_count ?? '') : ''}"></div>
        </div>
      </div>

      <div class="checkline"><input type="checkbox" id="f_out_rtmp" ${hasRTMPOut ? 'checked' : ''}> RTMP / RTMPS (push out, e.g. YouTube/Twitch)</div>
      <div id="f_out_rtmp_fields" class="sub-fields ${hasRTMPOut ? '' : 'hidden'}">
        <div class="field"><label>Target URL (use rtmps:// for TLS; stream key as last path segment)</label><input type="text" id="f_out_rtmp_url" value="${escapeAttr(hasRTMPOut ? hasRTMPOut.url : '')}" placeholder="rtmps://a.rtmp.youtube.com/live2/xxxx-xxxx-xxxx-xxxx"></div>
      </div>

      <div class="checkline"><input type="checkbox" id="f_out_webrtc" ${hasWebRTCOut ? 'checked' : ''}> WebRTC (WHEP play — video only)</div>
      <div id="f_out_webrtc_fields" class="sub-fields ${hasWebRTCOut ? '' : 'hidden'}">
        <div class="field"><label>Path</label><input type="text" id="f_out_webrtc_path" value="${escapeAttr(hasWebRTCOut ? hasWebRTCOut.path : '')}" placeholder="/whep/channel"></div>
        <div class="empty-note">A browser POSTs an SDP offer here to start watching. Video (H.264) only; a stream's audio, if any, isn't sent.</div>
      </div>

      <div class="field" style="margin-top:10px;">
        <label>Startup mode</label>
        <select id="f_mode">
          <option value="static" ${s.mode !== 'on_demand' ? 'selected' : ''}>Static — input runs continuously while enabled</option>
          <option value="on_demand" ${s.mode === 'on_demand' ? 'selected' : ''}>On demand — input starts when a viewer connects, stops when idle</option>
        </select>
        <div class="empty-note">On demand only gates pull-style inputs (HLS/DASH/RTSP/SRT-caller/UDP/DVB) when watched via HTTP-TS, HLS/DASH, or WHEP. A UDP/SRT/RTSP/RTMP output, or a WHIP input, has no per-viewer signal to gate on and keeps the input running continuously regardless of this setting.</div>
      </div>

      <div class="checkline" style="margin-top:10px;"><input type="checkbox" id="f_enabled" ${s.enabled ? 'checked' : ''}> Enabled</div>

      <div class="modal-actions">
        <button type="button" class="btn secondary" id="modalCancel">Cancel</button>
        <button type="submit" class="btn">${editing ? 'Save changes' : 'Add stream'}</button>
      </div>
    </form>
  </div>`;
}

document.getElementById('addStreamBtn').addEventListener('click', () => openStreamModal(null));

// ---------- status / test-links modal ----------
async function openStatusModal(id) {
  const s = state.streams.find(x => x.id === id);
  if (!s) return;

  const backdrop = document.getElementById('modalBackdrop');
  backdrop.innerHTML = renderStatusModal(s, null);
  backdrop.classList.remove('hidden');
  wireStatusModal(s);

  try {
    const status = await api('/api/status');
    const live = (status.streams || []).find(x => x.id === id) || null;
    backdrop.innerHTML = renderStatusModal(s, live);
    wireStatusModal(s);
  } catch (err) {
    // Leave the "loading" render in place; openStatusModal's own toast-free
    // failure is fine here, api() already redirected to login on 401.
  }
}

function wireStatusModal(s) {
  document.getElementById('modalCancel').addEventListener('click', closeModal);
  document.addEventListener('keydown', escToClose);
  const refreshBtn = document.getElementById('statusRefresh');
  if (refreshBtn) refreshBtn.addEventListener('click', () => openStatusModal(s.id));
  const switchBtn = document.getElementById('statusSwitchSource');
  if (switchBtn) {
    switchBtn.addEventListener('click', async () => {
      const sel = document.getElementById('statusSourceSelect');
      const index = Number(sel.value);
      try {
        await api(`/api/streams/${encodeURIComponent(s.id)}/switch-source`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ index }),
        });
        toast('Source switch requested');
        openStatusModal(s.id);
      } catch (err) {
        toast('Switch failed: ' + err.message, true);
      }
    });
  }
}

// stripScheme removes a leading "proto://" if present. The Listen address
// field expects a bare "host:port", but if someone pastes a full URL into
// it by mistake, host/portFromAddr should still parse something sane
// instead of doubling up (e.g. "rtmp://rtmp://host/port").
function stripScheme(addr) {
  return (addr || '').replace(/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//, '');
}

// splitHostPort pulls "host" and "port" (port may be "" if addr didn't
// specify one, e.g. a bare host with no ":port" at all) out of a Listen
// address value, tolerating a leading "proto://" and/or trailing "/path"
// in case one was pasted in by mistake — the field itself only wants
// "host:port".
function splitHostPort(addr) {
  const hostPort = stripScheme(addr).split('/')[0];
  const idx = hostPort.lastIndexOf(':');
  return idx >= 0 ? [hostPort.slice(0, idx), hostPort.slice(idx + 1)] : [hostPort, ''];
}

// isMulticastIP reports whether ip falls in the IPv4 multicast range
// (224.0.0.0-239.255.255.255).
function isMulticastIP(ip) {
  const first = parseInt((ip || '').split('.')[0], 10);
  return first >= 224 && first <= 239;
}

// isBindAll reports whether an address's host part is a wildcard bind
// (0.0.0.0, ::, or empty), in which case the browser's own hostname is a
// better guess for how to actually reach this server than the bind address.
function isBindAll(addr) {
  const [host] = splitHostPort(addr);
  return host === '' || host === '0.0.0.0' || host === '::';
}

function hostFromAddr(addr) {
  const [host] = splitHostPort(addr);
  return isBindAll(addr) ? location.hostname : host;
}
function portFromAddr(addr) {
  return splitHostPort(addr)[1];
}
function ensureSlash(p) {
  return p && p.endsWith('/') ? p : (p || '/') + '/';
}

function inputLink(input) {
  const proto = location.protocol;
  const httpHost = location.host;
  const i = input || {};
  switch (i.type) {
    case 'rtmp':
    case 'rtmps': {
      const port = portFromAddr(i.addr);
      const app = i.rtmp_app || 'live';
      return `${i.type}://${hostFromAddr(i.addr)}${port ? ':' + port : ''}/${app}/${i.rtmp_stream_key || '<any>'}`;
    }
    case 'srt':
      if (i.srt_mode === 'listener') {
        let url = `srt://${hostFromAddr(i.addr)}:${portFromAddr(i.addr)}?mode=caller`;
        if (i.srt_stream_id) url += `&streamid=${encodeURIComponent(i.srt_stream_id)}`;
        return url;
      }
      return `srt://${i.addr} (caller — we dial out, nothing to push here)`;
    case 'udp':
    case 'rtp':
      return `${i.type}://${i.addr}`;
    case 'httpts':
    case 'hls':
    case 'dash':
    case 'rtsp':
    case 'rtmp_pull':
      return i.url || '(pulls from a source, no local ingest point)';
    case 'dvb':
      return `DVB tuner ${i.dvb_adapter ?? 0}:${i.dvb_frontend ?? 0}`;
    case 'webrtc':
      return `${proto}//${httpHost}${i.path || ''} (WHIP — POST an SDP offer here to publish)`;
    default:
      return '';
  }
}

function outputLink(o) {
  const proto = location.protocol;
  const httpHost = location.host;
  switch (o.type) {
    case 'httpts':
      return { label: 'HTTP-TS', url: `${proto}//${httpHost}${o.path}`, clickable: true };
    case 'hls':
      return { label: 'HLS', url: `${proto}//${httpHost}${ensureSlash(o.path)}playlist.m3u8`, clickable: true };
    case 'dash':
      return { label: 'DASH', url: `${proto}//${httpHost}${ensureSlash(o.path)}manifest.mpd`, clickable: true };
    case 'udp': {
      // "@" before the host is the standard ffmpeg/VLC convention for a
      // multicast receive address (udp://@239.1.1.1:5000) — it tells the
      // client to join/bind the multicast group, not just connect to a
      // plain destination. Only meaningful (and only added) for an
      // actual multicast destination; a unicast one is just a normal
      // udp://host:port.
      const [ip] = splitHostPort(o.addr);
      const host = isMulticastIP(ip) ? '@' + o.addr : o.addr;
      return { label: o.rtp ? 'UDP/RTP' : 'UDP', url: `udp://${host}`, clickable: false };
    }
    case 'srt': {
      let url;
      let note = '';
      if (o.srt_mode === 'caller') {
        url = `srt://${o.addr}`;
        note = '(we push to this destination — nothing to pull from here)';
      } else {
        url = `srt://${hostFromAddr(o.addr)}:${portFromAddr(o.addr)}?mode=caller`;
        if (o.srt_stream_id) url += `&streamid=${encodeURIComponent(o.srt_stream_id)}`;
      }
      return { label: 'SRT', url, clickable: false, note };
    }
    case 'rtsp':
      return { label: 'RTSP', url: `rtsp://${hostFromAddr(o.addr)}:${portFromAddr(o.addr)}/`, clickable: false };
    case 'rtmp': {
      const secure = (o.url || '').startsWith('rtmps://');
      return { label: secure ? 'RTMPS' : 'RTMP', url: o.url, clickable: false, note: '(we push out to this target)' };
    }
    case 'webrtc':
      return { label: 'WebRTC (WHEP)', url: `${proto}//${httpHost}${o.path}`, clickable: false, note: '(POST an SDP offer here to watch — video only)' };
    default:
      return { label: o.type, url: '', clickable: false };
  }
}

function renderStatusModal(s, live) {
  const running = live ? live.running : null;
  const statusBadge = running === null
    ? `<span class="pill dim">Loading…</span>`
    : !running
      ? `<span class="pill bad">Stopped</span>`
      : live.on_demand && !live.input_active
        ? `<span class="pill dim">Idle (on demand — waiting for a viewer)</span>`
        : `<span class="pill good">Running</span>`;

  const sourceCount = live ? live.source_count : (1 + (s.backup_inputs || []).length);
  const sources = [s.input, ...(s.backup_inputs || [])];
  let sourceRows = '';
  if (sourceCount > 1) {
    const options = sources.map((src, idx) => {
      const activeMark = live && live.active_source === idx ? ' (active)' : '';
      const label = idx === 0 ? 'Primary' : `Backup #${idx}`;
      return `<option value="${idx}" ${live && live.active_source === idx ? 'selected' : ''}>${label}: ${escapeHtml(src.type)}${activeMark}</option>`;
    });
    options.push(`<option value="-1">Automatic failover</option>`);
    sourceRows = `
      <div class="field">
        <label>Active source ${live && live.source_pinned ? '<span class="badge">manually pinned</span>' : ''}</label>
        <div class="row" style="grid-template-columns: 1fr auto;">
          <select id="statusSourceSelect">${options.join('')}</select>
          <button type="button" class="btn secondary" id="statusSwitchSource">Switch</button>
        </div>
      </div>`;
  }

  const errorRow = live && live.source_error
    ? `<div class="error-text">Last error: ${escapeHtml(live.source_error)}</div>`
    : '';

  const ingestLink = inputLink(s.input);
  const ingestRow = ingestLink
    ? `<div class="field"><label>Ingest point</label><input type="text" readonly value="${escapeAttr(ingestLink)}" onclick="this.select()"></div>`
    : '';

  const statsRow = live
    ? `<div class="stat-row" style="margin:10px 0 4px;">
        <div class="stat"><span class="n">${live.clients}</span><span class="l">Clients</span></div>
        <div class="stat"><span class="n">${formatBps(live.input_bps)}</span><span class="l">Input bandwidth</span></div>
        <div class="stat"><span class="n">${formatBps(live.output_bps)}</span><span class="l">Output bandwidth</span></div>
      </div>`
    : '';

  const outputRows = (s.outputs || []).map(o => {
    const link = outputLink(o);
    const body = link.clickable && link.url
      ? `<a href="${escapeAttr(link.url)}" target="_blank" rel="noopener">${escapeHtml(link.url)}</a>`
      : `<input type="text" readonly value="${escapeAttr(link.url)}" onclick="this.select()">`;
    return `
      <div class="field">
        <label>${escapeHtml(link.label)} ${link.note ? `<span class="secondary">${escapeHtml(link.note)}</span>` : ''}</label>
        ${body}
      </div>`;
  }).join('') || `<p class="empty-note">No outputs configured.</p>`;

  return `
  <div class="modal">
    <h3>${escapeHtml(s.name || s.id)} — Status</h3>

    <div class="section-label">Input</div>
    <div class="field">
      <label>Type</label>
      <div>${statusBadge} <span class="badge">${escapeHtml(s.input ? s.input.type : '—')}</span></div>
    </div>
    ${ingestRow}
    ${sourceRows}
    ${errorRow}
    ${statsRow}

    <div class="section-label">Outputs</div>
    ${outputRows}

    <div class="modal-actions">
      <button type="button" class="btn secondary" id="statusRefresh">Refresh</button>
      <button type="button" class="btn secondary" id="modalCancel">Close</button>
    </div>
  </div>`;
}

// ---------- logs ----------
const logState = { es: null, paused: false, lines: [], filter: '' };

async function loadLogs() {
  logState.lines = [];
  document.getElementById('logLines').innerHTML = '';
  try {
    const resp = await api('/api/logs');
    logState.lines = resp.lines || [];
    renderLogLines();
    connectLogEvents(resp.last_seq || 0);
  } catch (err) {
    toast('Failed to load logs: ' + err.message, true);
  }
}

function connectLogEvents(since) {
  disconnectLogEvents();
  logState.es = new EventSource(`/api/logs/events?since=${encodeURIComponent(since)}`);
  logState.es.onmessage = (e) => {
    if (logState.paused) return;
    let lines;
    try { lines = JSON.parse(e.data); } catch { return; }
    logState.lines.push(...lines);
    const MAX = 5000;
    if (logState.lines.length > MAX) logState.lines.splice(0, logState.lines.length - MAX);
    renderLogLines();
  };
}

function disconnectLogEvents() {
  if (logState.es) { logState.es.close(); logState.es = null; }
}

function logLevelClass(text) {
  const t = text.toLowerCase();
  if (t.includes('panic') || t.includes('error') || t.includes('failed') || t.includes('fatal')) return 'lvl-error';
  if (t.includes('warn') || t.includes('retry') || t.includes('reject')) return 'lvl-warn';
  return '';
}

function formatLogTime(tsMs) {
  const d = new Date(tsMs);
  const pad = (n) => String(n).padStart(2, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function renderLogLines() {
  const wrap = document.getElementById('logLines');
  const viewport = document.getElementById('logViewport');
  if (!wrap || !viewport) return;
  const nearBottom = viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight < 60;

  const filter = logState.filter.toLowerCase();
  wrap.innerHTML = logState.lines.map(l => {
    const visible = !filter || l.text.toLowerCase().includes(filter);
    return `<div class="log-line ${logLevelClass(l.text)} ${visible ? '' : 'hidden-filtered'}"><span class="t">${formatLogTime(l.ts_ms)}</span><span class="m">${escapeHtml(l.text)}</span></div>`;
  }).join('');

  if (nearBottom) viewport.scrollTop = viewport.scrollHeight;
}

document.getElementById('logFilter').addEventListener('input', (e) => {
  logState.filter = e.target.value;
  renderLogLines();
});
document.getElementById('logPauseBtn').addEventListener('click', (e) => {
  logState.paused = !logState.paused;
  e.target.textContent = logState.paused ? 'Resume' : 'Pause';
});
document.getElementById('logClearBtn').addEventListener('click', () => {
  logState.lines = [];
  renderLogLines();
});

// ---------- settings ----------
async function loadSettings() {
  state.settings = await api('/api/settings');
  document.getElementById('s_admin_username').textContent = state.settings.admin_username;
  document.getElementById('s_listen_addr').textContent = state.settings.listen_addr;
  document.getElementById('s_allowed_ips').value = (state.settings.allowed_ips || []).join('\n');
  document.getElementById('s_epg_sources').value = (state.settings.epg_sources || []).join('\n');
}

document.getElementById('settingsForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const allowed_ips = document.getElementById('s_allowed_ips').value.split('\n').map(s => s.trim()).filter(Boolean);
  const epg_sources = document.getElementById('s_epg_sources').value.split('\n').map(s => s.trim()).filter(Boolean);
  const newPass = document.getElementById('s_new_password').value;
  try {
    await api('/api/settings', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ allowed_ips, epg_sources, new_password: newPass || undefined }),
    });
    document.getElementById('s_new_password').value = '';
    toast('Settings saved');
  } catch (err) {
    toast('Failed to save settings: ' + err.message, true);
  }
});

// ---------- boot ----------
api('/api/status').then(showApp).catch(() => showLogin());
