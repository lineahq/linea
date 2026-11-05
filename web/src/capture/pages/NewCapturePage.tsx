import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Link, useBlocker, useNavigate, useSearch } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { Collector } from '../../api/types';
import { Icon } from '../../ui/Icon';
import { Crumbs } from '../../ui/PageHead';
import {
  MAX_COLLECTORS,
  MAX_DURATION_SECONDS,
  MAX_TOTAL_BYTES,
  MAX_TOTAL_RECORDS,
  buildSelection,
  filterLimitsFor,
  defaultForm,
  formFromSelection,
  isFormDirty,
  parseDuration,
  formatSize,
  pointRows,
  reachableTargets,
  reconcilePoints,
  samplingOptions,
  samplingText,
  formatDuration,
  type CaptureForm,
  type FieldProblem,
} from '../captureForm';
import { connectionChip, isCapturable, locationText, setupState } from '../collectorView';
import { formatCount } from '../format';
import { loadCollectorCatalogs, loadCollectorInventory, loadFleetGroupMembers } from '../groupCapture';
import { initialTargetMode, parseNewCaptureSearch } from '../newCaptureSearch';
import { chipClass } from '../phase';
import { InlineConfirm } from './InlineConfirm';
import { useDenial } from '../../ui/session';

type Mode = 'group' | 'pick';
interface Targets {
  mode: Mode;
  groupId: string;
  pickIds: string[];
}

/** New capture: choose targets, points, a filter and limits, review, and start. */
export function NewCapturePage() {
  const navigate = useNavigate();
  const client = useQueryClient();
  // The route has no validateSearch yet, so the raw search is normalised here.
  const rawSearch = useSearch({ strict: false }) as Record<string, unknown>;
  const searchKey = JSON.stringify(rawSearch);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const search = useMemo(() => parseNewCaptureSearch(rawSearch), [searchKey]);

  const [targets, setTargets] = useState<Targets>(() => ({ mode: initialTargetMode(search) === 'group' ? 'group' : 'pick', groupId: search.group ?? '', pickIds: search.collectors ?? [] }));
  // A retarget held back until the owner agrees to it, because chosen points may no longer apply.
  const [retarget, setRetarget] = useState<Targets>();
  const [form, setForm] = useState<CaptureForm>(defaultForm);
  const [resetting, setResetting] = useState(false);
  const [filterText, setFilterText] = useState('');
  const [checked, setChecked] = useState<string>();

  const firstPage = useQuery({ queryKey: ['collectors', ''], queryFn: () => api.collectors(''), refetchInterval: 5000, refetchIntervalInBackground: false });
  const setup = setupState(firstPage.data);
  const groups = useQuery({ queryKey: ['collector-groups', ''], queryFn: () => api.collectorGroups(''), enabled: setup === 'ready' });
  const inventory = useQuery({ queryKey: ['capture-inventory'], queryFn: loadCollectorInventory, enabled: setup === 'ready', refetchInterval: 10000, refetchIntervalInBackground: false });
  const members = useQuery({
    queryKey: ['capture-group-members', targets.groupId],
    queryFn: () => loadFleetGroupMembers(targets.groupId, inventory.data),
    enabled: targets.mode === 'group' && !!targets.groupId && !!inventory.data,
    retry: false,
  });
  const source = useQuery({ queryKey: ['capture-session', search.from], queryFn: () => api.capture(search.from ?? ''), enabled: !!search.from, retry: false });

  // Duplicate: take the earlier capture's Collectors once, unless the link already named some.
  const seededTargets = useRef(false);
  useEffect(() => {
    if (seededTargets.current || !source.data || search.collectors || search.group) return;
    seededTargets.current = true;
    const from = source.data.selection;
    setTargets(from.group_id ? { mode: 'group', groupId: from.group_id, pickIds: [] } : { mode: 'pick', groupId: '', pickIds: from.collector_ids ?? [] });
  }, [source.data, search.collectors, search.group]);

  const resolved: Collector[] = useMemo(() => {
    if (targets.mode === 'group') return members.data?.collectors ?? [];
    const wanted = new Set(targets.pickIds);
    return (inventory.data ?? []).filter((c) => wanted.has(c.id));
  }, [targets, members.data, inventory.data]);
  const usable = resolved.filter(isCapturable);
  const unusable = resolved.length - usable.length;
  const usableKey = usable.map((c) => c.id).join(',');

  const catalogs = useQuery({
    queryKey: ['capture-catalogs', usableKey],
    queryFn: () => loadCollectorCatalogs(usable),
    enabled: usable.length > 0,
    staleTime: 15000,
  });
  const group = targets.mode === 'group';
  const rows = useMemo(() => pointRows(usable, catalogs.data ?? [], group), [usable, catalogs.data, group]);
  const offered = useMemo(() => rows.filter((r) => r.selectable).map((r) => r.point), [rows]);
  // Choices are only read through the catalog, so a point a target stops reporting is not sent;
  // it comes back if the target does.
  const effective = catalogs.data ? reconcilePoints(form, offered) : form;
  const hidden = form.points.length - effective.points.length;
  // A group capture reaches only the members that report every chosen point.
  const reached = group ? reachableTargets(usable, catalogs.data ?? [], effective.points) : usable;
  const built = buildSelection(group ? { groupId: targets.groupId, reachable: reached.length } : usable.map((c) => c.id), offered, effective, filterLimitsFor(reached, group));

  // Duplicate: seed the rest of the form once, from the earlier request, over a pristine form.
  const seededForm = useRef(false);
  useEffect(() => {
    if (seededForm.current || !source.data || !catalogs.data) return;
    seededForm.current = true;
    setForm((current) => {
      if (isFormDirty(current)) return current;
      const seeded = formFromSelection(source.data.selection, offered);
      baseline.current = seeded;
      return seeded;
    });
  }, [source.data, catalogs.data, offered]);

  // Leaving with choices the user made asks first (the form is not kept anywhere); starting the capture does not.
  const baseline = useRef<CaptureForm>(defaultForm);
  const started = useRef(false);
  const unsaved = !started.current && (JSON.stringify(form) !== JSON.stringify(baseline.current) || targets.pickIds.length !== (search.collectors ?? []).length);
  const blocker = useBlocker({
    shouldBlockFn: () => unsaved && !started.current,
    disabled: !unsaved,
    enableBeforeUnload: unsaved,
    withResolver: true,
  });

  const captureDenied = useDenial('capture.start');
  const start = useMutation({
    mutationFn: () => api.createCapture(built.selection!),
    onSuccess: async (session) => {
      started.current = true;
      client.setQueryData(['capture-session', session.id], session);
      await client.invalidateQueries({ queryKey: ['captures'] });
      void navigate({ to: '/captures/$captureId', params: { captureId: session.id } });
    },
  });
  const check = useMutation({
    mutationFn: () => api.validateCapture(built.selection!),
    onSuccess: () => setChecked('Checked: the platform accepts this capture and its filters.'),
    onError: () => setChecked(undefined),
  });

  function changeTargets(next: Targets) {
    if (effective.points.length > 0) setRetarget(next);
    else setTargets(next);
  }
  function togglePick(id: string) {
    setTargets((t) => {
      const has = t.pickIds.includes(id);
      if (!has && t.pickIds.length >= MAX_COLLECTORS) return t;
      return { ...t, mode: 'pick', pickIds: has ? t.pickIds.filter((x) => x !== id) : [...t.pickIds, id] };
    });
    setChecked(undefined);
  }
  const set = (patch: Partial<CaptureForm>) => {
    setForm({ ...form, ...patch });
    setChecked(undefined);
  };
  const problem = (field: FieldProblem['field']) => built.problems.find((p) => p.field === field)?.message;

  const notReady = setup === 'not-configured' || setup === 'waiting';
  const canStart = !!built.selection && !notReady && !start.isPending;
  const shownInventory = (inventory.data ?? []).filter((c) => !filterText.trim() || `${c.pod} ${c.cluster} ${c.namespace} ${c.collector_group}`.toLowerCase().includes(filterText.trim().toLowerCase()));
  const signals = built.signals.length ? built.signals : [...new Set(rows.filter((r) => effective.points.includes(r.key)).map((r) => r.point.signal))];
  const seconds = parseDuration(form.duration);

  return (
    <>
      <header className="od-wbhead">
        <Crumbs items={[{ label: 'Live capture', to: '/captures' }, { label: 'New capture' }]} />
        <span style={{ flex: 1 }} />
        {search.from && <span className="od-subtle" style={{ fontSize: 12.5 }}>Copy of an earlier capture</span>}
        <Link to="/captures" className="od-btn od-btn--sm">Cancel</Link>
      </header>
      {blocker.status === 'blocked' && (
        <div className="od-callout od-callout--warn od-callout--bar" role="alertdialog" aria-label="Leave New capture">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">Leave without starting the capture?</span>
            <span>The targets, points and limits you chose are not kept.</span>
          </div>
          <div className="od-callout__actions">
            <button type="button" className="od-btn od-btn--sm od-btn--primary" autoFocus onClick={() => blocker.reset()}>Keep editing</button>
            <button type="button" className="od-btn od-btn--danger od-btn--sm" onClick={() => blocker.proceed()}>Discard and leave</button>
          </div>
        </div>
      )}
      <div className="od-page">
        {firstPage.isPending && <p role="status" className="od-muted">Loading…</p>}
        {notReady && (
          <div className="od-callout od-callout--warn" role="status">
            <Icon name="triangle-alert" size={16} />
            <div className="od-callout__main">
              <div className="od-callout__title">{setup === 'waiting' ? 'No Collector has connected yet' : 'Live capture is not set up'}</div>
              <div className="od-callout__body">{setup === 'waiting' ? 'A capture needs at least one Collector connected to fleet with the livecapture extension.' : <>Start the server with <code>--fleet-url</code> and <code>--fleet-token-file</code> to capture through fleet.</>}</div>
            </div>
          </div>
        )}
        {search.from && source.error && (
          <div className="od-callout od-callout--warn" role="alert" style={{ marginBottom: 12 }}>
            <Icon name="triangle-alert" size={16} />
            <div className="od-callout__main">
              <div className="od-callout__title">Could not copy that capture</div>
              <div className="od-callout__body">{source.error.message}. Set the capture up from scratch below.</div>
            </div>
          </div>
        )}
        {setup === 'ready' && (
          <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) 320px', gap: 20, alignItems: 'start', maxWidth: 1200 }}>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 16, minWidth: 0 }}>
              {retarget && (
                <InlineConfirm
                  confirmLabel="Change targets"
                  cancelLabel="Keep targets"
                  onConfirm={() => { setTargets(retarget); setRetarget(undefined); }}
                  onCancel={() => setRetarget(undefined)}
                >
                  You have chosen {effective.points.length} capture point{effective.points.length === 1 ? '' : 's'}. Changing the targets may make some of them unavailable; those are left out until the targets report them again.
                </InlineConfirm>
              )}

              <StepCard n={1} title="Targets">
                <div role="radiogroup" aria-label="How to choose targets" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
                  <label className={`od-radio-card${targets.mode === 'group' ? ' od-radio-card--on' : ''}`}>
                    <input type="radio" name="target-mode" checked={targets.mode === 'group'} onChange={() => changeTargets({ ...targets, mode: 'group' })} />
                    <span><strong>Group</strong><br /><span className="od-subtle">Capture a fleet group of agents</span></span>
                  </label>
                  <label className={`od-radio-card${targets.mode === 'pick' ? ' od-radio-card--on' : ''}`}>
                    <input type="radio" name="target-mode" checked={targets.mode === 'pick'} onChange={() => changeTargets({ ...targets, mode: 'pick' })} />
                    <span><strong>Pick Collectors</strong><br /><span className="od-subtle">Choose from {inventory.data ? inventory.data.filter(isCapturable).length : '…'} connected</span></span>
                  </label>
                </div>
                {targets.mode === 'group' && (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                    <div className="od-field" style={{ maxWidth: 360 }}>
                      <label htmlFor="target-group">Group</label>
                      <select id="target-group" className="od-select" value={targets.groupId} onChange={(e) => changeTargets({ ...targets, groupId: e.target.value })}>
                        <option value="">Choose a group…</option>
                        {(groups.data?.items ?? []).map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
                      </select>
                    </div>
                    {groups.data && !groups.data.items?.length && <span className="od-field__help">Fleet has no groups yet. Create one in fleet, or pick Collectors instead.</span>}
                    {members.isFetching && !members.data && <span role="status" className="od-muted">Resolving members…</span>}
                    {members.error && <span role="alert" className="od-field__error">{members.error.message}</span>}
                    {!!members.data?.offline && <span className="od-field__help">{members.data.offline} member agent{members.data.offline === 1 ? ' is' : 's are'} offline and left out.</span>}
                    {targets.groupId && <span className="od-field__help">Fleet resolves the group again when the capture starts. Members that report every chosen point are captured; the others are left out.</span>}
                  </div>
                )}
                {targets.mode === 'pick' && (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                    <div className="od-search" style={{ maxWidth: 360 }}>
                      <Icon name="search" size={14} />
                      <input aria-label="Find Collectors" placeholder="Find by pod, cluster or namespace" value={filterText} onChange={(e) => setFilterText(e.target.value)} />
                    </div>
                    {inventory.isPending && <span role="status" className="od-muted">Loading Collectors…</span>}
                    {inventory.error && <span role="alert" className="od-field__error">{inventory.error.message}</span>}
                    <div role="group" aria-label="Collectors" style={{ display: 'flex', flexDirection: 'column', gap: 2, maxHeight: 260, overflow: 'auto' }}>
                      {shownInventory.map((c) => {
                        const on = targets.pickIds.includes(c.id);
                        const capturable = isCapturable(c);
                        const full = !on && targets.pickIds.length >= MAX_COLLECTORS;
                        const chip = connectionChip(c);
                        return (
                          <label key={c.id} style={{ display: 'flex', gap: 8, alignItems: 'center', padding: '4px 2px', fontSize: 13, opacity: capturable ? 1 : 0.6 }}>
                            <input type="checkbox" checked={on} disabled={(!capturable && !on) || full} onChange={() => togglePick(c.id)} />
                            <span className="mono">{c.pod}</span>
                            <span className="od-subtle">{locationText(c)}</span>
                            <span style={{ flex: 1 }} />
                            {!capturable && <span className={chipClass(chip.tone)}>{chip.label}</span>}
                          </label>
                        );
                      })}
                    </div>
                    <span className="od-field__help" role="status">{targets.pickIds.length} of {MAX_COLLECTORS} chosen{targets.pickIds.length >= MAX_COLLECTORS ? '. One capture supports at most 16 Collectors.' : ''}</span>
                  </div>
                )}
                {resolved.length > 0 && (
                  <span className="od-field__help" role="status">Resolved now: {usable.length} Collector{usable.length === 1 ? '' : 's'}{unusable ? `, ${unusable} with capture disabled and left out` : ''}.</span>
                )}
              </StepCard>

              <StepCard n={2} title="Capture points and signals">
                {!usable.length && <p className="od-muted" style={{ margin: 0 }}>Choose targets to see the points they report.</p>}
                {catalogs.isFetching && !catalogs.data && usable.length > 0 && <p role="status" className="od-muted" style={{ margin: 0 }}>Reading capture points…</p>}
                {catalogs.error && <p role="alert" className="od-field__error" style={{ margin: 0 }}>{catalogs.error.message}</p>}
                {hidden > 0 && <p role="status" className="od-field__help" style={{ margin: 0 }}>{hidden} chosen point{hidden === 1 ? ' is' : 's are'} left out because not every target reports {hidden === 1 ? 'it' : 'them'}.</p>}
                {rows.length > 0 && (
                  <table className="od-table" aria-label="Capture points">
                    <thead><tr><th style={{ width: 28 }}><span className="sr-only">Selected</span></th><th>Capture point</th><th>Signal</th><th>Available on</th></tr></thead>
                    <tbody>
                      {rows.map((r) => {
                        const on = effective.points.includes(r.key);
                        return (
                          <tr key={r.key}>
                            <td>
                              <input
                                type="checkbox"
                                aria-label={`${r.point.pipeline} ${r.point.point} ${r.point.signal}`}
                                checked={on}
                                disabled={!r.selectable}
                                onChange={() => set({ points: on ? form.points.filter((k) => k !== r.key) : [...form.points, r.key] })}
                              />
                            </td>
                            <td className="mono">{r.point.pipeline} · {r.point.point}{r.point.sensitivity === 'raw' && <span className="od-chip od-chip--warn" style={{ marginLeft: 6 }} title="Needs raw permission in fleet">raw</span>}</td>
                            <td>{r.point.signal}</td>
                            <td className={r.selectable ? undefined : 'od-subtle'}>{r.availableOn} of {usable.length}{r.reason ? ` · ${r.reason}` : ''}</td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                )}
                {rows.some((r) => r.point.sensitivity === 'raw') && <span className="od-field__help">Raw points capture unredacted data. Fleet starts them only when it grants you raw permission.</span>}
                {problem('points') && usable.length > 0 && rows.length > 0 && <span className="od-field__help">{problem('points')}</span>}
              </StepCard>

              <StepCard n={3} title="Filter and sampling">
                <Conditions id="f-resource" label="Resource (OTTL)" value={form.resource} onChange={(v) => set({ resource: v })} />
                {signals.includes('logs') && <Conditions id="f-logs" label="Log records (OTTL)" value={form.logs} onChange={(v) => set({ logs: v })} />}
                {signals.includes('traces') && <Conditions id="f-traces" label="Spans (OTTL)" value={form.traces} onChange={(v) => set({ traces: v })} />}
                {signals.includes('metrics') && <Conditions id="f-metrics" label="Metrics (OTTL)" value={form.metrics} onChange={(v) => set({ metrics: v })} />}
                {signals.includes('metrics') && <Conditions id="f-datapoint" label="Data points (OTTL)" value={form.datapoint} onChange={(v) => set({ datapoint: v })} />}
                <span className="od-field__help">Keep records where every condition holds, one per line. The Collector applies the filter before records are sent here.</span>
                {problem('filter') && <span role="alert" className="od-field__error">{problem('filter')}</span>}
                <div className="od-field" style={{ maxWidth: 280 }}>
                  <label htmlFor="sampling">Sampling</label>
                  <select id="sampling" className="od-select" value={String(form.samplingRate)} onChange={(e) => set({ samplingRate: Number(e.target.value) })}>
                    {samplingOptions(form.samplingRate).map((o) => <option key={o.rate} value={String(o.rate)}>{o.label}</option>)}
                  </select>
                  <span className="od-field__help">Applied per capture point.</span>
                </div>
              </StepCard>

              <StepCard n={4} title="Duration and limits">
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))', gap: 12 }}>
                  <TextField id="c-duration" label="Duration" value={form.duration} error={problem('duration')} onChange={(v) => set({ duration: v })} />
                  <TextField id="c-records" label="Max records" value={form.maxRecords} error={problem('maxRecords')} onChange={(v) => set({ maxRecords: v })} />
                  <TextField id="c-size" label="Max size" value={form.maxSize} error={problem('maxSize')} onChange={(v) => set({ maxSize: v })} />
                </div>
                <span className="od-field__help">The capture stops at the first limit reached. A capture lasts 5 seconds to {formatDuration(MAX_DURATION_SECONDS)}, up to {formatCount(MAX_TOTAL_RECORDS)} records and {formatSize(MAX_TOTAL_BYTES)}.</span>
              </StepCard>
            </div>

            <aside aria-label="Review" className="od-card" style={{ position: 'sticky', top: 12 }}>
              <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Review</h2></div>
              <div className="od-card__body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
                <div>
                  <div className="od-subtle" style={{ fontSize: 12, marginBottom: 4 }}>Targets resolved now</div>
                  {!reached.length && <span className="od-muted">{group && usable.length ? 'Choose points to see which members are reached' : 'None yet'}</span>}
                  <ul style={{ margin: 0, padding: 0, listStyle: 'none', display: 'flex', flexDirection: 'column', gap: 2 }}>
                    {reached.map((c) => (
                      <li key={c.id} style={{ display: 'flex', gap: 8, fontSize: 13 }}>
                        <span className="mono">{c.pod}</span>
                        <span className="od-subtle">{rows.filter((r) => effective.points.includes(r.key)).length} point{rows.filter((r) => effective.points.includes(r.key)).length === 1 ? '' : 's'}</span>
                      </li>
                    ))}
                  </ul>
                </div>
                <dl className="od-kv">
                  <dt>Signals</dt>
                  <dd>{signals.length ? signals.join(', ') : '—'}</dd>
                  <dt>Filter</dt>
                  <dd className="mono" style={{ wordBreak: 'break-word' }}>{filterSummary(effective)}</dd>
                  <dt>Sampling</dt>
                  <dd>{samplingText(form.samplingRate)}</dd>
                  <dt>Stops after</dt>
                  <dd>{seconds !== undefined ? formatDuration(seconds) : '—'} or {form.maxRecords || '—'} records or {form.maxSize || '—'}</dd>
                  <dt>Stored</dt>
                  <dd>On this machine only</dd>
                </dl>
                <div className="od-callout od-callout--info">
                  <Icon name="info" size={16} />
                  <div className="od-callout__main">
                    <div className="od-callout__title">What capturing does</div>
                    <div className="od-callout__body">Copies of matching records are sent here through fleet. The Collectors keep sending to their real exporters as before.</div>
                  </div>
                </div>
                {built.problems.length > 0 && (
                  <ul aria-label="Before you can start" style={{ margin: 0, paddingLeft: 18, fontSize: 12.5, color: 'var(--fg-muted)' }}>
                    {built.problems.map((p, i) => <li key={i}>{p.message}</li>)}
                  </ul>
                )}
                {checked && <span role="status" className="od-field__help">{checked}</span>}
                {check.error && <span role="alert" className="od-field__error">{check.error.message}</span>}
                {start.error && <span role="alert" className="od-field__error">{start.error.message}</span>}
                {resetting && (
                  <InlineConfirm danger confirmLabel="Reset form" cancelLabel="Keep my choices" onConfirm={() => { setForm(defaultForm); setResetting(false); setChecked(undefined); }} onCancel={() => setResetting(false)}>
                    Clear the capture points, filter, sampling and limits you set?
                  </InlineConfirm>
                )}
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  <button type="button" className="od-btn od-btn--primary" disabled={!canStart || !!captureDenied} title={captureDenied} onClick={() => start.mutate()}>
                    <Icon name="radio" size={16} />
                    <span>{start.isPending ? 'Starting…' : 'Start capture'}</span>
                  </button>
                  <button type="button" className="od-btn" disabled={!built.selection || check.isPending || !!captureDenied} title={captureDenied} onClick={() => check.mutate()}>Check only</button>
                  <button type="button" className="od-btn od-btn--ghost" disabled={!isFormDirty(form) || resetting} onClick={() => setResetting(true)}>Reset</button>
                </div>
              </div>
            </aside>
          </div>
        )}
      </div>
    </>
  );
}

function StepCard({ n, title, children }: { n: number; title: string; children: ReactNode }) {
  return (
    <section className="od-card" aria-labelledby={`step-${n}`}>
      <div className="od-card__head">
        <span className="od-step__dot" aria-hidden="true" style={{ width: 22, height: 22, fontSize: 12 }}>{n}</span>
        <h2 className="od-card__title" id={`step-${n}`} style={{ margin: 0 }}>{title}</h2>
      </div>
      <div className="od-card__body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>{children}</div>
    </section>
  );
}

function Conditions({ id, label, value, onChange }: { id: string; label: string; value: string; onChange: (v: string) => void }) {
  return (
    <div className="od-field">
      <label htmlFor={id}>{label}</label>
      <textarea id={id} className="od-input mono" rows={2} spellCheck={false} placeholder='e.g. severity_number >= 17' value={value} onChange={(e) => onChange(e.target.value)} />
    </div>
  );
}

function TextField({ id, label, value, error, onChange }: { id: string; label: string; value: string; error?: string; onChange: (v: string) => void }) {
  return (
    <div className="od-field">
      <label htmlFor={id}>{label}</label>
      <input id={id} className="od-input" value={value} aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-err` : undefined} onChange={(e) => onChange(e.target.value)} />
      {error && <span id={`${id}-err`} className="od-field__error" role="alert">{error}</span>}
    </div>
  );
}

/** filterSummary is the review panel's one-line filter: each condition, or "none". */
function filterSummary(form: CaptureForm): string {
  const all = [form.resource, form.logs, form.traces, form.metrics, form.datapoint]
    .flatMap((t) => t.split('\n'))
    .map((l) => l.trim())
    .filter(Boolean);
  return all.length ? all.join(' and ') : 'none';
}
