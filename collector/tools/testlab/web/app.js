'use strict';
const $ = id => document.getElementById(id);
let state = null, entries = [], after = 0, selected = null, busy = false, viewPaused = false, viewRevision = 0;
const number = n => Number(n || 0).toLocaleString();
async function api(path, method = 'GET', body) {
  const options = {method, headers: {}};
  if (body !== undefined) {options.headers['Content-Type'] = 'application/json'; options.body = JSON.stringify(body);}
  const response = await fetch(path, options);
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
  return data;
}
function showError(error) {$('message').textContent = error.message || String(error); $('message').hidden = false;}
function session() {return state?.collector?.sessions.find(s => s.state === 'active' || s.state === 'stopping') || state?.collector?.sessions.at(-1);}
function renderState() {
  if (!state) return;
  const collector = state.collector, generator = state.generator, buffer = state.buffer, current = session();
  const active = current?.state === 'active' || current?.state === 'stopping';
  const ready = collector?.enabled && collector.capture_points.length === 6;
  $('connection').textContent = state.collector_error ? 'Collector unavailable' : ready ? 'Collector ready' : 'Collector starting…';
  $('connection').className = `badge ${state.collector_error ? 'bad' : ready ? 'on' : ''}`;
  $('connection').title = state.collector_error || '';
  $('generated').textContent = number(generator.traces + generator.logs + generator.metrics);
  $('generator-state').textContent = `${generator.enabled ? 'Running' : 'Paused'} · ${generator.rate} samples/sec per signal`;
  $('received').textContent = number(buffer.received);
  $('retained').textContent = `${number(buffer.entries)} entries buffered · ${(buffer.bytes / 1024).toFixed(1)} KiB`;
  const title = active ? current.state === 'stopping' ? 'Stopping' : 'Active' : current?.state === 'expired' ? 'Expired' : 'Off';
  $('capture-state').textContent = title; $('capture-badge').textContent = title;
  $('capture-badge').className = `badge ${active ? 'on' : ''}`;
  $('session-time').textContent = active ? `${Math.max(0, Math.ceil((Date.parse(current.expires_at) - Date.now()) / 1000))} seconds remaining` : current ? `${current.state} · ${current.reason || 'session ended'}` : 'Start a capture below';
  $('session-id').textContent = current?.session_id || 'No session';
  $('dropped').textContent = number(Object.values(current?.stats?.dropped || {}).reduce((sum, n) => sum + n, 0));
  $('selected').textContent = current ? `${number(current.stats.selected)} selected · ${number(current.stats.enqueued_records)} enqueued` : 'No session selected';
  $('start').disabled = busy || !ready || active;
  $('stop').disabled = busy || !active;
  $('generator-toggle').disabled = busy; $('apply-rate').disabled = busy;
  $('generator-toggle').textContent = generator.enabled ? 'Pause generator' : 'Resume generator';
  $('generator-error').textContent = generator.last_error ? `Generator: ${generator.last_error}` : 'One span, one log, and one metric point per sample.';
  if (buffer.evicted > 0 || buffer.rejected > 0) {
    $('buffer-notice').hidden = false;
    $('buffer-notice').textContent = `${number(buffer.evicted)} older copies evicted from the bounded viewer buffer; ${number(buffer.rejected)} untagged or oversized records refused.`;
  }
}
function inspect(item) {
  selected = item;
  $('detail-meta').textContent = `${item.signal} · ${item.service || '(no service)'} · ${item.pipeline}/${item.point} · ${item.session_id}`;
  $('detail').textContent = JSON.stringify(item.otlp, null, 2); $('copy').disabled = false;
  for (const row of $('rows').children) {row.classList.toggle('selected', row.dataset.id === String(item.id)); row.setAttribute('aria-selected', row.dataset.id === String(item.id));}
}
function renderEntries() {
  const filter = $('view-signal').value, search = $('search').value.toLowerCase();
  const visible = entries.filter(e => (!filter || e.signal === filter) && `${e.service} ${e.point} ${e.summary} ${e.session_id}`.toLowerCase().includes(search));
  const fragment = document.createDocumentFragment();
  for (const item of visible.slice().reverse()) {
    const row = document.createElement('tr'); row.tabIndex = 0; row.dataset.id = item.id;
    row.classList.toggle('selected', selected?.id === item.id); row.setAttribute('aria-selected', selected?.id === item.id);
    const values = [new Date(item.received_at).toLocaleTimeString(), item.signal, item.service || '—', item.point, item.summary];
    for (let i = 0; i < values.length; i++) {
      const cell = document.createElement('td');
      if (i === 1) {const badge = document.createElement('span'); badge.className = `signal ${item.signal}`; badge.textContent = values[i]; cell.append(badge);}
      else {cell.textContent = values[i]; cell.title = values[i];}
      row.append(cell);
    }
    row.addEventListener('click', () => inspect(item));
    row.addEventListener('keydown', event => {if (event.key === 'Enter' || event.key === ' ') {event.preventDefault(); inspect(item);}});
    fragment.append(row);
  }
  $('rows').replaceChildren(fragment); $('empty').hidden = visible.length > 0;
  $('row-count').textContent = `${number(visible.length)} entries`;
}
async function action(work) {
  if (busy) return;
  busy = true; $('message').hidden = true; renderState();
  try {await work(); state = await api('/api/state');}
  catch (error) {showError(error);}
  finally {busy = false; renderState();}
}
$('capture-form').addEventListener('submit', event => {
  event.preventDefault();
  action(() => api('/api/capture', 'POST', {
    signals: [...document.querySelectorAll('input[name="signals"]:checked')].map(e => e.value),
    points: [...document.querySelectorAll('input[name="points"]:checked')].map(e => e.value),
    service: $('service').value, errors_only: $('errors-only').checked,
    ottl: Object.fromEntries(['resource', 'span', 'log', 'metric', 'datapoint'].map(context =>
      [context, $(`ottl-${context}`).value.split('\n').map(line => line.trim()).filter(Boolean)])),
    duration_seconds: Number($('duration').value), sampling_rate: Number($('sampling').value) / 100
  }));
});
$('stop').addEventListener('click', () => action(() => api('/api/capture', 'DELETE')));
$('generator-toggle').addEventListener('click', () => action(() => api('/api/generator', 'POST', {enabled: !state.generator.enabled, rate: state.generator.rate})));
$('apply-rate').addEventListener('click', () => action(() => api('/api/generator', 'POST', {enabled: state.generator.enabled, rate: Number($('rate').value)})));
$('pause-view').addEventListener('click', () => {viewPaused = !viewPaused; $('pause-view').textContent = viewPaused ? 'Resume view' : 'Pause view'; $('live').textContent = viewPaused ? 'PAUSED' : 'LIVE'; $('live').classList.toggle('paused', viewPaused);});
$('clear').addEventListener('click', () => action(async () => {
	viewRevision++;
  await api('/api/captures', 'DELETE'); entries = []; after = 0; selected = null;
  $('detail').textContent = 'No record selected.'; $('detail-meta').textContent = 'Select a row to inspect its resource, scope, and record.'; $('copy').disabled = true; renderEntries();
}));
$('view-signal').addEventListener('change', renderEntries); $('search').addEventListener('input', renderEntries);
$('copy').addEventListener('click', async () => {try {await navigator.clipboard.writeText(JSON.stringify(selected.otlp, null, 2)); $('copy').textContent = 'Copied'; setTimeout(() => {$('copy').textContent = 'Copy JSON';}, 1200);} catch (error) {showError(error);}});
async function poll() {
  try {
    state = await api('/api/state'); renderState();
    if (!viewPaused && !busy) {
      const revision = viewRevision;
      let changed = false;
      // Fetch enough pages to catch up with the synthetic generator, while
      // bounding work per poll and rendering the table only once.
      for (let pages = 0; pages < 3; pages++) {
        const page = await api(`/api/captures?after=${after}`);
        if (revision !== viewRevision || viewPaused || busy) return;
        if (page.missed) {$('buffer-notice').hidden = false; $('buffer-notice').textContent = 'Some older entries expired from the viewer buffer before this view could fetch them.';}
        if (page.entries.length) {entries = entries.concat(page.entries).slice(-500); after = page.next_after; changed = true;}
        if (page.entries.length < 200) break;
      }
      if (changed) renderEntries();
    }
  } catch (error) {$('connection').textContent = 'Test lab unavailable'; $('connection').className = 'badge bad'; $('start').disabled = true; $('stop').disabled = true;}
  finally {setTimeout(poll, 750);}
}
poll();
