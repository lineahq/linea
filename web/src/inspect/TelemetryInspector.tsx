import { useMemo, useState } from 'react';
import type { Aggregation, Event, Filtering, OTTLReport, RecordLineage, Routing, Stage, StageInput } from '../api/types';
import type { WorkspaceSearch } from '../router';
import type { LayoutNode } from '../graph/layout';
import { eventNodeKey, stageNodeKey } from '../runs/annotations';
import { useSnapshot, useSnapshots } from './useSnapshot';
import { concatPayloads, deferredWindow, eventRecord, resourceCount, type DeferredWindow } from './deferred';
import { listRecords, pretty, type RecordEntry } from './records';
import { nextSteps, prevStep, type Step } from './navigation';
import { findLineage } from './journey';
import { VERIFICATION } from './aggregation';
import { AggregationList } from './AggregationList';
import { RoutingList } from './RoutingList';
import { explained } from './routing';
import { removedBy, removedByText } from './filtering';
import { RecordChanges } from './RecordChanges';
import { JsonTree } from './JsonTree';
import { OTTLPanel } from '../ottl/OTTLPanel';
import { SignalExplorer } from './SignalExplorer';
import { RawJSONDiff } from './RawJSONDiff';
import { capOptions } from './pages';
import { payloadGroups, payloadOption, payloadSummary } from './payloads';

interface Props {
  runId: string;
  node: LayoutNode;
  events: Event[];
  stages?: Stage[];
  /** OTTL replays of transform stage inputs. */
  ottl?: OTTLReport[];
  /** Record journeys of the run, to follow a selected record. */
  lineage?: RecordLineage[];
  /** What aggregating connector inputs were counted into. */
  aggregations?: Aggregation[];
  /** Which rule of a routing connector sent each record where. */
  routing?: Routing[];
  /** Which condition of a filter processor removed each record. */
  filtering?: Filtering[];
  live: boolean;
  search: WorkspaceSearch;
  setSearch(patch: Partial<WorkspaceSearch>): void;
}

/** What a payload view lists: injected inputs, exports, or what an extension's simulated endpoints received. */
const PAYLOAD_KINDS: Record<string, string> = { receiver: 'input.injected', exporter: 'exporter.captured', extension: 'endpoint.received' };

const PAYLOAD_VIEWS = [
  ['tree', 'Payload tree'],
  ['json', 'Raw JSON'],
] as const;

/**
 * TelemetryInspector shows what a node saw in a run: for a processor, each
 * stage input with its output and diff; for a receiver, the injected
 * payloads; for an exporter, the captured ones; for a connector, where each
 * input was routed, or for an aggregating one what it was counted into. Payloads are fetched by snapshot ref only when shown.
 */
export function TelemetryInspector(p: Props) {
  if (p.node.component.kind === 'processor') return <StageView {...p} />;
  if (p.node.component.kind === 'connector') return <ConnectorView {...p} />;
  if (['receiver', 'exporter', 'extension'].includes(p.node.component.kind)) return <PayloadView {...p} />;
  return <div className="placeholder">Select a processor, connector, receiver or exporter.</div>;
}

function go(setSearch: Props['setSearch'], s: Step) {
  setSearch({ node: s.node, input: s.input, record: undefined, tab: 'telemetry', view: undefined });
}

/**
 * StepButtons move along the payload: back to what handed it over, forward
 * to what consumed it, one button per branch where it fanned out.
 */
function StepButtons({ prev, next, setSearch }: { prev?: Step; next: Step[]; setSearch: Props['setSearch'] }) {
  return (
    <span className="step-nav">
      <button
        type="button"
        disabled={!prev}
        title={prev ? `From ${prev.label}` : 'Nothing observed handed over this payload'}
        onClick={() => prev && go(setSearch, prev)}
      >
        ← Previous
      </button>
      {next.length <= 1 ? (
        <button
          type="button"
          disabled={!next[0]}
          title={next[0] ? `To ${next[0].label}` : 'Nothing observed consumed this payload'}
          onClick={() => next[0] && go(setSearch, next[0])}
        >
          Next →
        </button>
      ) : (
        next.map((n) => (
          <button key={n.input} type="button" title={`To ${n.label}`} onClick={() => go(setSearch, n)}>
            → {n.label}
          </button>
        ))
      )}
    </span>
  );
}

/** FollowButton opens the journey of the selected record, if the run followed it. */
function FollowButton({
  lineage,
  seq,
  record,
  setSearch,
}: {
  lineage?: RecordLineage[];
  seq: number;
  record?: string;
  setSearch: Props['setSearch'];
}) {
  if (!record) return null;
  const l = findLineage(lineage, seq, record);
  if (!l) return null;
  return (
    <button
      type="button"
      className="follow"
      title="Follow this record through the whole run"
      onClick={() => setSearch({ trace: l.id, tab: 'journey' })}
    >
      Follow record ⇢
    </button>
  );
}

function TechnicalDetails({ views, view, set }: { views: readonly (readonly [string, string])[]; view: string; set(v: string): void }) {
  if (['changes', 'ottl', 'routes'].includes(view)) return null;
  const technical = view !== 'explore';
  return (
    <div className="technical-tools">
      {technical ? <><button type="button" onClick={() => set('explore')}>← Back to readable data</button>
        <label>Technical format <select aria-label="Technical format" value={view} onChange={(e) => set(e.target.value)}>
          {views.map(([k, label]) => <option key={k} value={k}>{label}</option>)}
        </select></label></> : <button type="button" className="link" onClick={() => set('json')}>Open technical details</button>}
    </div>
  );
}

function RecordPicker({
  records,
  record,
  onRecord,
  highlight,
}: {
  records: RecordEntry[];
  record?: string;
  onRecord(r: string | undefined): void;
  highlight?: Set<string>;
}) {
  const { shown, hidden } = capOptions(records);
  return (
    <select aria-label="Record" value={record ?? ''} onChange={(e) => onRecord(e.target.value || undefined)}>
      <option value="">All records ({records.length})</option>
      {shown.map((r) => (
        <option key={r.entity} value={r.entity}>
          {highlight?.has(r.entity) ? '● ' : ''}
          {r.label || r.entity} — {r.entity}
        </option>
      ))}
      {hidden > 0 && <option disabled value="-">{hidden} more records: find them in the readable data</option>}
      {record && !shown.some((r) => r.entity === record) && <option value={record}>{record}</option>}
    </select>
  );
}

function StageView({ runId, node, events, stages, ottl, filtering, lineage, live, search, setSearch }: Props) {
  const stage = stages?.find((s) => stageNodeKey(s) === node.key);
  const bySeq = useMemo(() => new Map(events.map((e) => [e.seq, e])), [events]);
  const liveInputs = useMemo(
    () => events.filter((e) => e.kind === 'stage.input' && eventNodeKey(e) === node.key),
    [events, node.key],
  );
  if (!stage) {
    if (live) return <div className="placeholder">{liveInputs.length} input(s) so far; diffs are available when the run finishes.</div>;
    return <div className="placeholder">This processor received no telemetry in this run.</div>;
  }
  const inputs = stage.inputs ?? [];
  const selected = inputs.find((i) => i.seq === search.input) ?? inputs[0];
  return (
    <div className="details telemetry">
      {inputs.length > 1 && <div className="stage-head">
          <select
            aria-label="Stage input"
            value={selected?.seq}
            onChange={(e) => setSearch({ input: Number(e.target.value), record: undefined })}
          >
            {inputs.map((i) => (
              <option key={i.seq} value={i.seq}>
                input #{i.seq} · {i.outcome} · {bySeq.get(i.seq)?.counts?.items ?? '?'} items
              </option>
            ))}
          </select>
      </div>}
      {selected?.outcome === 'deferred' && deferredWindow(stage) ? (
        <DeferredView runId={runId} stage={stage} window={deferredWindow(stage)!} bySeq={bySeq} lineage={lineage} search={search} setSearch={setSearch} />
      ) : selected ? (
        <StageInputView
          key={selected.seq}
          runId={runId}
          input={selected}
          stage={stage}
          events={events}
          bySeq={bySeq}
          ottl={ottl?.find((r) => r.processor === stage.component && r.input === selected.seq)}
          filtering={filtering?.find((f) => f.processor === stage.component && f.pipeline === node.pipeline && f.input === selected.seq)}
          lineage={lineage}
          search={search}
          setSearch={setSearch}
        />
      ) : (
        <p className="muted">No inputs.</p>
      )}
    </div>
  );
}

const OUTCOME_NOTES: Record<string, string> = {
  emitted: 'The processor passed this input on.',
  no_output: 'The processor emitted nothing for this input: everything in it was dropped.',
  deferred: 'The processor buffered this input and emitted it later, possibly merged with others; there is no per-input diff.',
  unresolved: 'The output for this input could not be attributed to it.',
};

/**
 * DeferredView compares everything a buffering processor received with
 * everything it emitted later (the stage's reconciliation), since its
 * output cannot be paired with one input.
 */
function DeferredView({ runId, stage, window, bySeq, lineage, search, setSearch }: {
  runId: string; stage: Stage; window: DeferredWindow; bySeq: Map<number, Event>;
  lineage?: RecordLineage[]; search: WorkspaceSearch; setSearch: Props['setSearch'];
}) {
  const before = useSnapshots(runId, window.inputs.map((seq) => bySeq.get(seq)?.snapshot));
  const after = useSnapshots(runId, window.emissions.map((seq) => bySeq.get(seq)?.snapshot));
  const merged = useMemo(() => ({
    before: before.data && concatPayloads(before.data),
    after: after.data && concatPayloads(after.data),
  }), [before.data, after.data]);
  const items = (seqs: number[]) => seqs.reduce((n, seq) => n + (bySeq.get(seq)?.counts?.items ?? 0), 0);
  const error = before.error ?? after.error;
  return <>
    <p className="note">
      {stage.type} buffers what it receives and emits it later on its own schedule, so its output cannot be paired with one input.
      This compares all {window.inputs.length} buffered input(s) ({items(window.inputs)} records) with
      all {window.emissions.length} later emission(s) ({items(window.emissions)} records).
      {stage.type === 'tail_sampling' && <> It keeps or drops whole traces by its policies, not by these values:{' '}
        <button type="button" className="link" onClick={() => setSearch({ tab: 'decisions', view: undefined, record: undefined })}>see why each trace was kept or dropped</button>.</>}
    </p>
    {error ? <p className="error">{error.message}</p> : before.isLoading || after.isLoading || !merged.before || !merged.after ? <p className="muted">Loading recorded data…</p> :
      <RecordChanges diff={stage.reconciliation} before={merged.before} after={merged.after} record={search.record}
        onClearRecord={() => setSearch({ record: undefined })}
        onFollow={(entity, side) => {
          const seqs = side === 'before' ? window.inputs : window.emissions;
          const at = eventRecord(entity, seqs, (side === 'before' ? before.data : after.data)!.map(resourceCount));
          const l = at && findLineage(lineage, at.seq, at.entity);
          return l && (() => setSearch({ trace: l.id, tab: 'journey', view: undefined }));
        }} />}
  </>;
}

function StageInputView({
  runId, input, bySeq, ottl, filtering, lineage, search, setSearch,
}: {
  runId: string; input: StageInput; stage: Stage; events: Event[];
  bySeq: Map<number, Event>; ottl?: OTTLReport; filtering?: Filtering; lineage?: RecordLineage[];
  search: WorkspaceSearch; setSearch: Props['setSearch'];
}) {
  const before = useSnapshot(runId, bySeq.get(input.seq)?.snapshot);
  const [output, setOutput] = useState(input.outputs?.[0]);
  const after = useSnapshot(runId, output === undefined ? undefined : bySeq.get(output)?.snapshot);
  const error = before.error ?? after.error;
  return <>
    {input.outcome !== 'emitted' && <p className="note">{OUTCOME_NOTES[input.outcome] ?? input.outcome}</p>}
    {input.error && <p className="error">Error: {input.error}</p>}
    {(input.outputs?.length ?? 0) > 1 && <label>Output <select aria-label="Output batch" value={output} onChange={(e) => setOutput(Number(e.target.value))}>
      {input.outputs!.map((seq, i) => <option key={seq} value={seq}>Batch {i + 1} · {bySeq.get(seq)?.counts?.items ?? '?'} records</option>)}
    </select></label>}
    {error ? <p className="error">{error.message}</p> : before.isLoading || after.isLoading ? <p className="muted">Loading recorded data…</p> :
      search.view === 'ottl' ? <><OTTLPanel report={ottl} input={before.data} observed={after.data}
        onRecord={(record) => setSearch({ record, tab: 'changes', view: undefined })}
        onRerun={(run) => setSearch({ run, input: undefined, record: undefined })}
        runId={runId} onCompare={(compare) => setSearch({ compare })} />
        <RawJSONDiff before={before.data} after={after.data} afterMissingReason={input.outcome === 'emitted' ? undefined : OUTCOME_NOTES[input.outcome]} /></> :
      <RecordChanges diff={output === input.outputs?.[0] ? input.diff : undefined} before={before.data} after={after.data}
        record={search.record} onClearRecord={() => setSearch({ record: undefined })}
        afterMissingReason={input.outcome === 'emitted' ? undefined : OUTCOME_NOTES[input.outcome]}
        removedBy={filtering && output === input.outputs?.[0] ? (entity) => removedByText(removedBy(filtering, entity)) : undefined}
        report={output === input.outputs?.[0] ? ottl : undefined} onOpenStatements={ottl ? () => setSearch({ tab: 'ottl', view: undefined }) : undefined}
        onFollow={(entity, side) => {
          const seq = side === 'before' ? input.seq : output;
          const l = seq === undefined ? undefined : findLineage(lineage, seq, entity);
          return l && (() => setSearch({ trace: l.id, tab: 'journey', view: undefined }));
        }} />}
  </>;
}

function PayloadView({ runId, node, events, lineage, search, setSearch }: Props) {
  const kind = PAYLOAD_KINDS[node.component.kind] ?? 'exporter.captured';
  const list = useMemo(() => events.filter((e) => e.kind === kind && eventNodeKey(e) === node.key), [events, kind, node.key]);
  const selected = list.find((e) => e.seq === search.input) ?? list[0];
  const snap = useSnapshot(runId, selected?.snapshot);
  const all = useSnapshots(runId, list.map((e) => e.snapshot));
  const groups = useMemo(() => payloadGroups(list), [list]);
  const records = useMemo(() => listRecords(snap.data), [snap.data]);
  const record = records.find((r) => r.entity === search.record);
  const value = record ? record.value : snap.data;
  const view = PAYLOAD_VIEWS.some(([k]) => k === search.view) ? search.view! : 'explore';
  const label = kind === 'input.injected' ? 'Received telemetry' : kind === 'endpoint.received' ? 'Sent telemetry' : 'Exported telemetry';
  if (!selected) {
    return (
      <div className="placeholder">
        {kind === 'input.injected' ? 'Nothing was injected at this receiver.'
          : kind === 'endpoint.received' ? 'No simulated endpoint of this extension received anything.' : 'Nothing reached this exporter.'}
      </div>
    );
  }
  const prev = kind === 'exporter.captured' ? prevStep(events, selected.seq) : undefined;
  const next = kind === 'input.injected' ? nextSteps(events, selected.seq, undefined) : [];
  return (
    <div className="details telemetry">
      <div className="stage-head">
        <span className="muted">{kind === 'input.injected' ? 'injected' : kind === 'endpoint.received' ? `sent to ${selected.endpoint}` : 'captured'}</span>
        {view !== 'explore' && list.length > 1 && (
          <select aria-label="Payload" value={selected.seq} onChange={(e) => setSearch({ input: Number(e.target.value), record: undefined })}>
            {list.map((e, i) => <option key={e.seq} value={e.seq}>{payloadOption(list, i)}</option>)}
          </select>
        )}
        {view !== 'explore' && <span className="muted">{payloadSummary(list, selected)}</span>}
        {view !== 'explore' && <RecordPicker records={records} record={search.record} onRecord={(r) => setSearch({ record: r })} />}
        {view !== 'explore' && <><FollowButton lineage={lineage} seq={selected.seq} record={search.record} setSearch={setSearch} />
        <StepButtons prev={prev} next={next} setSearch={setSearch} /></>}
      </div>
      {selected.error && <p className="error">Error: {selected.error}</p>}
      {view !== 'explore' && <TechnicalDetails views={PAYLOAD_VIEWS} view={view} set={(v) => setSearch({ view: v })} />}
      {view === 'explore' ? (
        all.error ? <p className="error">{all.error.message}</p>
        : all.isLoading || !all.data ? <p className="muted">Loading payloads…</p>
        : groups.map((g) => (
          <SignalExplorer
            simple
            key={g.signal}
            payload={undefined}
            requests={g.indexes.map((i) => all.data![i])}
            recordRequest={g.indexes.indexOf(list.indexOf(selected)) + 1 || undefined}
            label={label}
            record={g.indexes.includes(list.indexOf(selected)) ? search.record : undefined}
            onClearRecord={() => setSearch({ record: undefined })}
            onFollow={(entity, request) => {
              const e = list[g.indexes[(request ?? 1) - 1]];
              const l = e && findLineage(lineage, e.seq, entity);
              return l && (() => setSearch({ trace: l.id, tab: 'journey', view: undefined }));
            }}
          />
        ))
      ) : snap.error ? <p className="error">{snap.error.message}</p>
      : snap.isLoading ? (
        <p className="muted">Loading payload…</p>
      ) : view === 'tree' ? (
        <JsonTree value={value} />
      ) : (
        <pre className="code raw">{pretty(value)}</pre>
      )}
      {view === 'explore' && all.data && all.data.map((d, i) => (
        <details key={list[i].seq}><summary>Raw OTLP JSON{list.length > 1 ? ` · request ${i + 1}` : ''}</summary><pre className="code">{pretty(d)}</pre></details>
      ))}
    </div>
  );
}

/**
 * ConnectorView shows where a connector sent each input: one route per
 * pipeline it delivered into, and which records each route carried. For an
 * aggregating connector (count) it shows the data points each input was
 * counted into instead.
 */
function ConnectorView({ runId, node, events, stages, lineage, aggregations, routing, live, search, setSearch }: Props) {
  const stage = stages?.find((s) => stageNodeKey(s) === node.key);
  const bySeq = useMemo(() => new Map(events.map((e) => [e.seq, e])), [events]);
  if (!stage) {
    return (
      <div className="placeholder">
        {live ? 'Routes are shown when the run finishes.' : 'This connector received no telemetry in this run.'}
      </div>
    );
  }
  const inputs = stage.inputs ?? [];
  const selected = inputs.find((i) => i.seq === search.input) ?? inputs[0];
  return (
    <div className="details telemetry">
      <div className="stage-head">
        <span>
          <span className="muted">
            Input #{selected?.seq ?? '—'} · {stage.signal}
          </span>
        </span>
        {inputs.length > 1 && (
          <select
            aria-label="Connector input"
            value={selected?.seq}
            onChange={(e) => setSearch({ input: Number(e.target.value), record: undefined })}
          >
            {inputs.map((i) => (
              <option key={i.seq} value={i.seq}>
                input #{i.seq} · {bySeq.get(i.seq)?.counts?.items ?? '?'} items · {i.routes?.length ?? 0} route(s)
              </option>
            ))}
          </select>
        )}
      </div>
      {selected && search.view !== 'routes' ? (
        <ConnectorSignals key={selected.seq} runId={runId} input={selected} bySeq={bySeq} lineage={lineage} setSearch={setSearch} />
      ) : selected && aggregations?.find((a) => a.input === selected.seq) ? (
        <AggregationView
          a={aggregations.find((a) => a.input === selected.seq)!}
          input={selected}
          items={bySeq.get(selected.seq)?.counts?.items}
          events={events}
          lineage={lineage}
          setSearch={setSearch}
        />
      ) : selected ? (
        <RoutesView input={selected} items={bySeq.get(selected.seq)?.counts?.items} events={events} lineage={lineage} routing={routing?.find((r) => r.input === selected.seq)} setSearch={setSearch} />
      ) : (
        <p className="muted">No inputs.</p>
      )}
    </div>
  );
}

function ConnectorSignals({ runId, input, bySeq, lineage, setSearch }: {
  runId: string;
  input: StageInput;
  bySeq: Map<number, Event>;
  lineage?: RecordLineage[];
  setSearch: Props['setSearch'];
}) {
  const [seq, setSeq] = useState(input.seq);
  const route = input.routes?.find((r) => r.seq === seq);
  const before = useSnapshot(runId, bySeq.get(input.seq)?.snapshot);
  const after = useSnapshot(runId, route ? bySeq.get(seq)?.snapshot : undefined);
  return <>
    <div className="data-direction"><label>Recorded payload <select aria-label="Connector payload" value={seq} onChange={(e) => setSeq(Number(e.target.value))}>
      <option value={input.seq}>Incoming · {bySeq.get(input.seq)?.counts?.items ?? '?'} items</option>
      {(input.routes ?? []).map((r) => <option key={r.seq} value={r.seq}>Outgoing · {r.pipeline} · {r.items} items</option>)}
    </select></label></div>
    <SnapshotSignals key={seq} runId={runId} event={bySeq.get(seq)} label={route ? `Outgoing · ${route.pipeline}` : 'Incoming'} lineage={lineage} setSearch={setSearch} />
    {!input.routes?.length && <p className="muted">No outgoing route was observed for this input.</p>}
    {!before.isLoading && !after.isLoading && <RawJSONDiff before={before.data} after={after.data}
      afterMissingReason={input.routes?.length ? 'Select an outgoing recorded payload above to compare it with the input.' : 'No outgoing route was observed. Only the recorded input is available.'} />}
  </>;
}

function SnapshotSignals({ runId, event, label, lineage, setSearch }: {
  runId: string;
  event?: Event;
  label: string;
  lineage?: RecordLineage[];
  setSearch: Props['setSearch'];
}) {
  const snap = useSnapshot(runId, event?.snapshot);
  if (snap.isLoading) return <p className="muted">Loading {label.toLowerCase()}…</p>;
  if (snap.error) return <p className="error">{snap.error.message}</p>;
  return <SignalExplorer simple payload={snap.data} label={label} onFollow={(entity) => {
    const l = event && findLineage(lineage, event.seq, entity);
    return l && (() => setSearch({ trace: l.id, tab: 'journey' }));
  }} />;
}

function RoutesView({
  input,
  items,
  events,
  lineage,
  routing,
  setSearch,
}: {
  input: StageInput;
  items?: number;
  events: Event[];
  lineage?: RecordLineage[];
  routing?: Routing;
  setSearch: Props['setSearch'];
}) {
  const routes = input.routes ?? [];
  const openRoute = (seq: number, pipeline: string) => {
    const n = nextSteps(events, seq, pipeline)[0];
    if (n) go(setSearch, n);
  };
  return (
    <>
      <p className="muted">
        {items ?? '?'} item(s) in ·{' '}
        {routes.length ? `delivered into ${routes.length} pipeline(s); a record delivered to several pipelines was copied` : 'delivered into no pipeline'}
      </p>
      {input.error && <p className="error">Error: {input.error}</p>}
      {routing && !explained(routing) && <p className="rv-notice">Which rule routed each record is not shown{routing.notes?.length ? `: ${routing.notes[0]}` : '.'}</p>}
      <RoutingList input={input} lineage={lineage} routing={routing} follow={(id) => setSearch({ trace: id, tab: 'journey' })} openRoute={openRoute} />
    </>
  );
}

/**
 * AggregationView lists the data points an aggregating connector emitted for
 * one input, the records each counted (when the replay verified them) and
 * the records no metric counted. Records and points link to their journeys.
 */
function AggregationView({
  a,
  input,
  items,
  events,
  lineage,
  setSearch,
}: {
  a: Aggregation;
  input: StageInput;
  items?: number;
  events: Event[];
  lineage?: RecordLineage[];
  setSearch: Props['setSearch'];
}) {
  const points = a.points ?? [];
  const [label, help] = VERIFICATION[a.verification] ?? [a.verification, ''];
  const routes = input.routes ?? [];
  const follow = (id: string) => setSearch({ trace: id, tab: 'journey' });
  const openRoute = (seq: number, pipeline: string) => {
    const n = nextSteps(events, seq, pipeline)[0];
    if (n) go(setSearch, n);
  };
  return (
    <>
      <p className="muted">
        {items ?? '?'} item(s) in · counted into {points.length} data point(s)
        {routes.length > 0 && (
          <>
            {' '}
            delivered into{' '}
            {routes.map((r, i) => (
              <span key={r.seq}>
                {i > 0 && ', '}
                <button type="button" className="link" title="Inspect what this pipeline did with it" onClick={() => openRoute(r.seq, r.pipeline)}>
                  {r.pipeline}
                </button>
              </span>
            ))}
          </>
        )}{' '}
        <span className={`verification verification-${a.verification}`} title={help}>
          {label}
        </span>
      </p>
      {input.error && <p className="error">Error: {input.error}</p>}
      {(a.notes ?? []).map((n, i) => (
        <p key={i} className="note small">
          {n}
        </p>
      ))}
      <AggregationList a={a} lineage={lineage} follow={follow} />
    </>
  );
}
