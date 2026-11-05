import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../api/client';
import type { CaptureSession, CaptureStream } from '../../api/types';
import { Icon } from '../../ui/Icon';
import { Crumbs } from '../../ui/PageHead';
import { CaptureMetadataEditor } from '../CaptureMetadataEditor';
import { samplingText } from '../captureForm';
import {
  canStop,
  captureTitle,
  dataNotice,
  deleteBlocker,
  filterLines,
  isReceiving,
  metricsView,
  pointLines,
  receivedDetail,
  storageNote,
  startedBy,
  stopAfterText,
  targetLabel,
} from '../detailView';
import { clockTime, formatCount } from '../format';
import { capturePhase, captureLifecycle, chipClass, sessionCounts, stepClass, targetChip } from '../phase';
import { groupSelection, selectionBarText, selectionProblem } from '../recordView';
import { CaptureRecords } from './CaptureRecords';
import { InlineConfirm } from './InlineConfirm';
import { UseRecordsDrawer } from './UseRecordsDrawer';
import { Permitted } from '../../ui/Permitted';
import { useDenial } from '../../ui/session';
import '../collectors.css';

/** useNow ticks every second while `live`, so the elapsed time keeps counting. */
function useNow(live: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!live) return;
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [live]);
  return now;
}

/** Capture detail: one capture's state, records, targets and provenance. */
export function CaptureDetailPage() {
  const { captureId } = useParams({ strict: false }) as { captureId?: string };
  const id = captureId ?? '';
  const navigate = useNavigate();
  const client = useQueryClient();
  const session = useQuery({
    queryKey: ['capture-session', id],
    queryFn: () => api.capture(id),
    enabled: !!id,
    retry: (count, error) => !(error instanceof ApiError && error.status === 404) && count < 2,
    // Poll while the capture is live or records can still arrive; stop once it is settled or failed to load.
    refetchInterval: (q) => {
      if (q.state.status === 'error') return false;
      const d = q.state.data;
      return !d || !d.terminal || isReceiving(d.data) ? 2000 : false;
    },
    refetchIntervalInBackground: false,
  });
  const s = session.data;
  const phase = s ? capturePhase(sessionCounts(s)) : undefined;
  const now = useNow(!!s && !s.terminal);

  const [confirm, setConfirm] = useState<'stop' | 'delete'>();
  const [selected, setSelected] = useState<Record<number, CaptureStream>>({});
  const [watermark, setWatermark] = useState(0);
  const [using, setUsing] = useState(false);
  const onWatermark = useCallback((w: number) => setWatermark(w), []);
  const groups = useMemo(() => groupSelection(selected, watermark), [selected, watermark]);
  const count = Object.keys(selected).length;
  const problem = selectionProblem(groups);

  const captureDenied = useDenial('capture.start');
  // Deriving inputs inspects the destination configuration (run.submit).
  const deriveDenied = useDenial('run.submit');
  const stop = useMutation({
    mutationFn: () => api.stopCapture(id),
    onSuccess: async (next) => {
      client.setQueryData(['capture-session', id], next);
      setConfirm(undefined);
      await client.invalidateQueries({ queryKey: ['captures'] });
    },
  });
  const remove = useMutation({
    mutationFn: () => api.deleteCapture(id),
    onSuccess: async () => {
      client.removeQueries({ queryKey: ['capture-session', id] });
      client.removeQueries({ queryKey: ['capture-records', id] });
      await client.invalidateQueries({ queryKey: ['captures'] });
      void navigate({ to: '/captures' });
    },
  });

  const missing = session.error instanceof ApiError && session.error.status === 404;
  if (missing || session.error) {
    return (
      <>
        <header className="od-wbhead">
          <Crumbs items={[{ label: 'Live capture', to: '/captures' }, { label: id }]} />
        </header>
        <div className="od-page">
          <div className="od-callout od-callout--fail" role="alert">
            <Icon name="circle-x" size={16} />
            <div className="od-callout__main">
              <div className="od-callout__title">{missing ? 'This capture no longer exists' : 'Could not load this capture'}</div>
              <div className="od-callout__body">{missing ? 'It was deleted. Records and payloads are gone; saved fixtures and runs keep their own copies.' : session.error?.message}</div>
              <div className="od-callout__actions">
                {!missing && <button type="button" className="od-btn od-btn--sm" onClick={() => void session.refetch()}>Try again</button>}
                <Link to="/captures" className="od-btn od-btn--sm">Back to captures</Link>
              </div>
            </div>
          </div>
        </div>
      </>
    );
  }
  if (!s || !phase) {
    return (
      <>
        <header className="od-wbhead"><Crumbs items={[{ label: 'Live capture', to: '/captures' }, { label: id }]} /></header>
        <div className="od-page"><p role="status" className="od-muted">Loading capture…</p></div>
      </>
    );
  }

  const blocker = deleteBlocker(s);
  const m = metricsView(s, now);
  const notice = dataNotice(s.data);
  const steps = captureLifecycle(sessionCounts(s));
  const filters = filterLines(s.selection.filter);

  return (
    <>
      <header className="od-wbhead" style={{ flexWrap: 'wrap' }}>
        <Crumbs items={[{ label: 'Live capture', to: '/captures' }, { label: captureTitle(s) }]} />
        <span className={chipClass(phase.tone)} role="status">
          <Icon name={phase.icon} size={12} />
          {phase.label}
        </span>
        <span style={{ flex: 1 }} />
        <Permitted perm="capture.start"><Link to="/captures/new" search={{ from: s.id }} className="od-btn od-btn--sm">
          <Icon name="copy" size={14} />
          <span>Duplicate</span>
        </Link></Permitted>
        <CaptureMetadataEditor session={s} buttonClassName="od-btn od-btn--sm" />
        <button type="button" className="od-btn od-btn--sm" disabled={!!captureDenied || !canStop(s) || confirm === 'stop'} onClick={() => setConfirm('stop')} title={captureDenied ?? (canStop(s) ? undefined : s.terminal ? 'This capture has finished.' : 'A stop has already been requested.')}>
          <Icon name="square" size={14} />
          <span>Stop</span>
        </button>
        <button type="button" className="od-btn od-btn--danger od-btn--sm" disabled={!!captureDenied || !!blocker || confirm === 'delete'} onClick={() => setConfirm('delete')} title={captureDenied ?? blocker}>
          <Icon name="trash-2" size={14} />
          <span>Delete</span>
        </button>
      </header>

      {confirm === 'stop' && (
        <div style={{ padding: '8px 16px 0' }}>
          <InlineConfirm danger busy={stop.isPending} confirmLabel="Stop capture" cancelLabel="Keep capturing" onConfirm={() => stop.mutate()} onCancel={() => setConfirm(undefined)}>
            Ask every Collector to stop this capture? Records already received are kept. Each Collector confirms separately, and this page shows who has.
            {stop.error && <div role="alert" style={{ color: 'var(--fail)' }}>{stop.error.message}</div>}
          </InlineConfirm>
        </div>
      )}
      {confirm === 'delete' && (
        <div style={{ padding: '8px 16px 0' }}>
          <InlineConfirm danger busy={remove.isPending} confirmLabel="Delete capture" cancelLabel="Keep capture" onConfirm={() => remove.mutate()} onCancel={() => setConfirm(undefined)}>
            Delete this capture and its received records? Saved fixtures and runs that used them keep their own copies.
            {remove.error && <div role="alert" style={{ color: 'var(--fail)' }}>{remove.error.message}</div>}
          </InlineConfirm>
        </div>
      )}
      {phase.headline && (
        <div className={`od-callout od-callout--bar ${phase.tone === 'fail' ? 'od-callout--fail' : 'od-callout--warn'}`} role="status">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{phase.headline}</span>
            {phase.detail && <div className="od-callout__body">{phase.detail}</div>}
          </div>
        </div>
      )}
      {notice && (
        <div className="od-callout od-callout--warn od-callout--bar" role="alert">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main"><span className="od-callout__title">{notice}</span></div>
        </div>
      )}

      <div style={{ position: 'relative', display: 'flex', flex: 1, minHeight: 0, overflow: 'hidden' }}>
        <div className="od-page" style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 14, overflow: 'auto' }}>
          {s.metadata?.description && <p className="od-muted" style={{ margin: 0 }}>{s.metadata.description}</p>}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))', gap: 12 }}>
            <Metric label="Collectors reported sending" value={formatCount(m.reported)} detail="enqueued for delivery; not proof of receipt" />
            <Metric label="Debugger received" value={formatCount(m.received)} detail={receivedDetail(m)} />
            <Metric label="Elapsed" value={m.elapsed.text} detail={`${m.elapsed.percent}% of the capture window`}>
              <div role="progressbar" aria-label="Capture window" aria-valuemin={0} aria-valuemax={100} aria-valuenow={m.elapsed.percent} style={{ height: 4, background: 'var(--border-subtle)', borderRadius: 2, marginTop: 6 }}>
                <div style={{ width: `${m.elapsed.percent}%`, height: '100%', background: 'var(--accent)', borderRadius: 2 }} />
              </div>
            </Metric>
            <Metric label="Stored" value={m.storage.headline} detail={m.storage.detail} title={storageNote(s.data)} />
          </div>

          {count > 0 && (
            <div className="od-callout od-callout--info od-callout--bar" role="status">
              <Icon name="file-text" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">{count} record{count === 1 ? '' : 's'} selected</span>
                <div className="od-callout__body">{problem ?? selectionBarText(groups)}</div>
              </div>
              <div className="od-callout__actions">
                <button type="button" className="od-btn od-btn--sm" onClick={() => setSelected({})}>Clear</button>
                <button type="button" className="od-btn od-btn--primary od-btn--sm" disabled={!!deriveDenied || !!problem || using} title={deriveDenied ?? problem} onClick={() => setUsing(true)}>Use as inputs</button>
              </div>
            </div>
          )}

          <CaptureRecords session={s} selected={selected} onSelected={setSelected} onWatermark={onWatermark} />
        </div>

        <aside aria-label="Capture details" className="capture-aside">
          <section aria-label="Lifecycle">
            <h2 className="od-card__title">Lifecycle</h2>
            {steps.map((step) => (
              <div className={stepClass(step.status)} key={step.id}>
                <span className="od-step__dot" aria-hidden="true">
                  {step.status === 'done' && <Icon name="circle-check" size={12} />}
                  {step.status === 'failed' && <Icon name="circle-x" size={12} />}
                  {step.status === 'warn' && <Icon name="triangle-alert" size={12} />}
                </span>
                <div>
                  <div className="od-step__title">{step.title}</div>
                  <div className="od-step__detail">{step.id === 'requested' ? startedBy(s.caller) : step.detail}</div>
                </div>
                <span className="od-step__time">{step.time ? clockTime(step.time) : ''}</span>
              </div>
            ))}
          </section>

          <section aria-label="Per target">
            <h2 className="od-card__title">Per target</h2>
            <table className="od-table capture-aside__table">
              <thead><tr><th>Collector</th><th className="num">Reported</th><th>State</th></tr></thead>
              <tbody>
                {(s.targets ?? []).map((t) => {
                  const chip = targetChip(t);
                  return (
                    <tr key={t.collector_id}>
                      <td className="mono capture-aside__clip" title={t.agent_id ? `${targetLabel(t)} · agent ${t.agent_id}` : targetLabel(t)}>{targetLabel(t)}</td>
                      <td className="num" title={t.stats?.filter_errors ? `${formatCount(t.stats.filter_errors)} filter errors` : undefined}>{t.stats ? formatCount(t.stats.enqueued_records) : '—'}</td>
                      <td><span className={chipClass(chip.tone)}><Icon name={chip.icon} size={12} />{chip.label}</span></td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            <p className="capture-aside__note">Reported by the Collectors; sampling and transport can make the received count differ.</p>
          </section>

          <Provenance session={s} filters={filters} />
        </aside>

        {using && <UseRecordsDrawer session={s} groups={groups} onClose={() => setUsing(false)} />}
      </div>
    </>
  );
}

function Metric({ label, value, detail, title, children }: { label: string; value: string; detail?: string; title?: string; children?: React.ReactNode }) {
  return (
    <div className="od-card" style={{ padding: '10px 14px' }} title={title}>
      <div className="od-subtle" style={{ fontSize: 12 }}>{label}</div>
      <div style={{ fontSize: 22, fontWeight: 600, margin: '2px 0' }}>{value}</div>
      {detail && <div className="od-subtle" style={{ fontSize: 12 }}>{detail}</div>}
      {children}
    </div>
  );
}

function Provenance({ session: s, filters }: { session: CaptureSession; filters: ReturnType<typeof filterLines> }) {
  const points = pointLines(s.selection);
  return (
    <section aria-label="Provenance">
      <h2 className="od-card__title">Provenance</h2>
      <dl className="od-kv">
        <dt>Capture points</dt>
        <dd className="mono">{points.length ? points.map((p) => <div key={p}>{p}</div>) : '—'}</dd>
        <dt>Signals</dt>
        <dd>{s.selection.signals?.join(', ') || '—'}</dd>
        <dt>Filter</dt>
        <dd className="mono" style={{ wordBreak: 'break-word' }}>
          {filters.length ? filters.map((f) => <div key={f.label}>{f.label}: {f.conditions.join(' and ')}</div>) : 'none'}
        </dd>
        <dt>Sampling</dt>
        <dd>{samplingText(s.selection.sampling?.rate ?? 1)}</dd>
        <dt>Stops after</dt>
        <dd>{stopAfterText(s.selection, s.requested_limits)}</dd>
        <dt>Started by</dt>
        <dd>{startedBy(s.caller)}</dd>
      </dl>
      {s.data.contains_raw && <p className="capture-aside__note">Includes raw payloads, stored unredacted.</p>}
    </section>
  );
}
