import type { GroupCaptureTargets } from './groupCapture';
import type { CapturePointPage, CaptureSelection, CaptureValidation, Collector } from '../api/types';
import { MAX_DURATION_SECONDS, formatDuration } from './captureForm';

type Point = NonNullable<CapturePointPage['items']>[number];
export function CaptureSelectionForm({ text, onChange, collector, groupTargets, points, busy, starting = false, disabled, validation, onValidate, onStart, pointsLoading, pointsError, pointPaging }: {
  text: string; onChange: (text: string) => void; collector?: Collector; groupTargets?: GroupCaptureTargets; points: Point[]; busy: boolean; starting?: boolean; disabled: boolean;
  validation?: CaptureValidation; onValidate: () => void; onStart: () => void; pointsLoading?: boolean; pointsError?: string; pointPaging?: React.ReactNode;
}) {
  let selection: CaptureSelection | undefined;
  try { const value = JSON.parse(text); if (value && typeof value === 'object' && value.limits && typeof value.limits === 'object' && value.filter && typeof value.filter === 'object' && (!value.capture_points || Array.isArray(value.capture_points) && value.capture_points.every((p: { pipeline?: unknown; point?: unknown }) => p && typeof p.pipeline === 'string' && typeof p.point === 'string'))) selection = value; } catch { /* The advanced editor retains incomplete JSON. */ }
  const update = (patch: Partial<CaptureSelection>) => onChange(JSON.stringify({ ...selection, ...patch }, null, 2));
  const chosen = selection?.capture_points ?? [];
  const signals = ['traces', 'logs', 'metrics'];
  function toggle(point: Point) {
    const selected = chosen.some(p => p.pipeline === point.pipeline && p.point === point.point);
    const next = selected ? chosen.filter(p => p.pipeline !== point.pipeline || p.point !== point.point) : [...chosen, { pipeline: point.pipeline, point: point.point }];
    const nextSignals = signals.filter(signal => next.some(p => p.pipeline.split('/')[0] === signal));
    update({ capture_points: next, signals: nextSignals });
  }
  const groupUnavailable = groupTargets?.collectors.some(item => !item.enabled);
  const groupMismatch = groupTargets && JSON.stringify([...(selection?.collector_ids ?? [])].sort()) !== JSON.stringify(groupTargets.collectors.map(item => item.id).sort());
  const unavailable = !!groupUnavailable || !!groupMismatch || collector && !collector.enabled;
  const duration = selection?.duration_seconds ?? 60;
  const sample = (selection?.sampling?.rate ?? 1) * 100;
  const groupSize = groupTargets?.collectors.length ?? 0;
  const groupSizeLabel = `${groupSize} collector${groupSize === 1 ? '' : 's'}`;
  const ready = !!selection && chosen.length > 0 && (selection.collector_ids?.length ?? 0) > 0 && (selection.collector_ids?.length ?? 0) <= 16 && chosen.length <= 64 && duration >= 5 && duration <= MAX_DURATION_SECONDS && sample >= 0 && sample <= 100;
  return <form className="capture-builder" onSubmit={e => { e.preventDefault(); if (ready && !busy && !disabled && !pointsLoading && !pointsError && !unavailable) onStart(); }}>
    <div className="capture-section-title"><div><span className="capture-eyebrow">2 · Configure</span><h2>{groupTargets ? `Capture from ${groupTargets.label}` : collector ? `Capture from ${collector.pod}` : 'Configure your capture'}</h2><p>Select where to collect data, then choose how long to capture.</p></div>{collector && <span className={`capture-badge ${collector.enabled ? 'is-good' : 'is-warning'}`}>{collector.enabled ? 'connected' : 'capture disabled'}</span>}</div>
    {unavailable && !groupTargets && <p className="capture-notice" role="status">This collector has capture disabled in its livecapture extension. Choose another collector.</p>}
    {groupTargets && <><p className="capture-help">One capture across {groupSizeLabel}. This target list is fixed; newly connecting collectors are not added. Start rechecks every target.</p><details className="capture-disclosure" open><summary>Review {groupSizeLabel}</summary><ul className="capture-target-list" aria-label="Capture targets">{groupTargets.collectors.map(item => <li key={item.id}><div><strong>{item.pod}</strong><small>{[item.cluster, item.namespace, item.collector_group].filter(Boolean).join(' / ')}</small></div><span className="capture-badge">{item.enabled ? 'connected' : 'capture disabled'}</span></li>)}</ul></details>{groupUnavailable && <p className="capture-notice" role="status">Every group member must have capture enabled. No collectors will be silently skipped. Choose another group or an individual collector.</p>}{groupMismatch && <p className="capture-notice" role="alert">Target IDs changed in Advanced request. Select the group again to review the new targets.</p>}</>}
    <fieldset className="capture-point-picker"><legend>Capture points <span className="capture-muted">{chosen.length} selected</span></legend><p className="capture-help">Ingress captures data before processing; egress captures it after processing.{groupTargets && ' Only points shared by every collector are shown. Raw points need raw permission in fleet.'}</p>
      {pointsLoading && <p role="status">Loading capture points…</p>}{pointsError && <p role="alert">{pointsError}</p>}
      {(collector || groupTargets) && !pointsLoading && !pointsError && !points.length && <p className="capture-empty">{groupTargets ? 'No capture points are shared by every group member. Choose a narrower group.' : 'No points reported yet. The collector may still be starting.'}</p>}
      {signals.filter(signal => points.some(p => p.signal === signal)).map(signal => <div className="capture-signal-group" key={signal}><h3><span className={`capture-signal-icon signal-${signal}`}>{signal === 'traces' ? '↗' : signal === 'logs' ? '≡' : '▥'}</span>{signal[0].toUpperCase() + signal.slice(1)}</h3><div className="capture-point-options">{points.filter(p => p.signal === signal).sort((a, b) => a.point === b.point ? a.pipeline.localeCompare(b.pipeline) : a.point === 'ingress' ? -1 : b.point === 'ingress' ? 1 : a.point.localeCompare(b.point)).map(point => {
        const blocked = !!unavailable;
        const checked = chosen.some(p => p.pipeline === point.pipeline && p.point === point.point);
        return <label className={`capture-point-option ${checked ? 'is-selected' : ''} ${blocked ? 'is-disabled' : ''}`} key={`${point.pipeline}/${point.point}/${point.signal}`}><input type="checkbox" checked={checked} disabled={blocked} onChange={() => toggle(point)} aria-label={`${point.signal} ${point.pipeline} ${point.point}`} /><span><strong>{point.point}</strong><small>{point.pipeline}</small></span><span className={`capture-badge ${point.sensitivity === 'raw' ? 'is-warning' : ''}`}>{point.sensitivity === 'raw' ? 'raw · needs fleet permission' : point.sensitivity}</span></label>;
      })}</div></div>)}
      {pointPaging}
      {!points.length && !collector && chosen.length > 0 && <p>{chosen.map(p => `${p.pipeline} / ${p.point}`).join(', ')}</p>}
    </fieldset>
    <div className="capture-field-row"><label>Duration<span className="capture-input-unit"><input aria-label="Duration in seconds" type="number" min={5} max={MAX_DURATION_SECONDS} required value={duration} onChange={e => update({ duration_seconds: Number(e.target.value) })} /><span>seconds</span></span><small>Capture stops automatically. Maximum {formatDuration(MAX_DURATION_SECONDS)}.</small></label><label>Sampling<span className="capture-input-unit"><input aria-label="Sampling percentage" type="number" min={0} max={100} step="any" required value={sample} onChange={e => update({ sampling: { mode: selection?.sampling?.mode || 'hash', rate: Number(e.target.value) / 100 } })} /><span>% of records</span></span><small>Use 100% to collect every matching record.</small></label></div>
    <details className="capture-disclosure"><summary>Filters & limits <span>Optional</span></summary><div className="capture-advanced-fields">
      <p className="capture-help">Limit the capture to records you need. Enter one OTTL condition per line; the collector validates the expressions.</p>
      {(['resource', 'traces', 'logs', 'metrics', 'datapoint'] as const).map(kind => {
        const filter = selection?.filter ?? {};
        const values = kind === 'resource' ? filter.resource : kind === 'traces' ? filter.traces?.span : kind === 'logs' ? filter.logs?.log : kind === 'metrics' ? filter.metrics?.metric : filter.metrics?.datapoint;
        const set = (value: string, clean = false) => {
          const list = clean ? value.split('\n').map(v => v.trim()).filter(Boolean) : value.split('\n');
          const next = kind === 'resource' ? { ...filter, resource: list } : kind === 'datapoint' ? { ...filter, metrics: { ...filter.metrics, datapoint: list } } : { ...filter, [kind]: { ...filter[kind], [kind === 'traces' ? 'span' : kind === 'logs' ? 'log' : 'metric']: list } };
          update({ filter: next });
        };
        return <label key={kind}>{kind === 'resource' ? 'Resource conditions' : kind === 'traces' ? 'Span conditions' : kind === 'logs' ? 'Log conditions' : kind === 'metrics' ? 'Metric conditions' : 'Datapoint conditions'}<textarea rows={2} value={(values ?? []).join('\n')} placeholder={kind === 'logs' ? 'severity_number >= 9' : 'Optional OTTL condition'} onChange={e => set(e.target.value)} onBlur={e => set(e.target.value, true)} /></label>;
      })}
      <div className="capture-field-row"><label>Record limit<input type="number" min={1} max={100000} value={selection?.limits.total_records ?? 100000} onChange={e => update({ limits: { ...selection!.limits, total_records: Number(e.target.value) } })} /></label><label>Byte limit<input type="number" min={1} value={selection?.limits.total_bytes ?? 67108864} onChange={e => update({ limits: { ...selection!.limits, total_bytes: Number(e.target.value) } })} /></label></div>
      <p className="capture-help">Limits apply per collector. The platform retains up to 10,000 records or 8 MiB per capture. Deployment limits may be lower.</p>
    </div></details>
    <details className="capture-disclosure" open={!selection && !!text}><summary>Advanced request <span>JSON</span></summary><p className="capture-help">For additional rate limits or custom selections. This is the same request used by the CLI and API.</p><label className="capture-sr-only" htmlFor="capture-selection">Capture selection JSON</label><textarea id="capture-selection" className="capture-json" rows={15} spellCheck={false} value={text} onChange={e => onChange(e.target.value)} /></details>
    {validation && <p className="capture-notice is-success" role="status">Preflight accepted for {validation.collectors?.length ?? 0} collectors. The collector confirms activation after you start.</p>}
    <footer className="capture-start-bar"><div><strong>{chosen.length ? `${chosen.length} point${chosen.length === 1 ? '' : 's'} · ${duration} seconds · ${sample}% sampling` : 'Choose at least one capture point'}</strong><span>{groupTargets ? `${groupSizeLabel} · limits apply per collector` : 'Capture starts only when you press Start capture.'}</span></div><div className="capture-actions"><button type="button" disabled={busy || disabled || pointsLoading || !!pointsError || !ready || !!unavailable} onClick={onValidate}>{busy && !starting ? 'Checking…' : 'Check selection'}</button><button className="primary" type="submit" disabled={busy || disabled || pointsLoading || !!pointsError || !ready || !!unavailable}>{starting ? 'Starting…' : 'Start capture'}</button></div></footer>
  </form>;
}
