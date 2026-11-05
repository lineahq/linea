import { useMemo, useState } from 'react';
import type { Change, Diff } from '../api/types';
import { display, groupChanges, refEntity, type Attribute, type EntityChanges, type RecordGroup } from './grouped';
import { changeOverview } from './changeOverview';
import { ChangesOverview } from './ChangesOverview';

const MARK: Record<string, string> = { added: '+', removed: '−', modified: '~', type_changed: '~' };

function ChangeRow({ c }: { c: Change }) {
  return (
    <li className={`chg chg-${c.op}`}>
      <span className="chg-mark" aria-label={c.op}>
        {MARK[c.op] ?? '~'}
      </span>
      <code className="chg-path">{c.path}</code>
      {c.op === 'added' ? (
        <code className="val after">{display(c.after)}</code>
      ) : c.op === 'removed' ? (
        <code className="val before">{display(c.before)}</code>
      ) : (
        <>
          <code className="val before">{display(c.before)}</code>
          <span className="muted">→</span>
          <code className="val after">{display(c.after)}</code>
        </>
      )}
      {c.op === 'type_changed' && (
        <span className="muted">
          {' '}
          ({c.before_type} → {c.after_type})
        </span>
      )}
      {c.confidence && c.confidence !== 'exact' && <span className={`conf conf-${c.confidence}`}>{c.confidence} match</span>}
    </li>
  );
}

function Unchanged({ attrs, all }: { attrs: Attribute[]; all: boolean }) {
  if (!attrs.length) return null;
  return all ? (
    <>
      {attrs.map((a) => (
        <li key={a.key} className="chg chg-same">
          <span className="chg-mark"> </span>
          <code className="chg-path">attributes["{a.key}"]</code>
          <code className="val">{a.value}</code>
        </li>
      ))}
    </>
  ) : (
    <li className="chg chg-same muted">{attrs.length} attribute(s) unchanged</li>
  );
}

function EntityBlock({ e, all }: { e: EntityChanges; all: boolean }) {
  return (
    <ul className="chg-list">
      {e.changes.map((c, i) => (
        <ChangeRow key={i} c={c} />
      ))}
      <Unchanged attrs={e.unchanged} all={all} />
    </ul>
  );
}

function RecordBlock({
  r,
  all,
  record,
  onRecord,
  onFollow,
}: {
  r: RecordGroup;
  all: boolean;
  record?: string;
  onRecord(e: string | undefined): void;
  onFollow?(entity: string): (() => void) | undefined;
}) {
  const follow = onFollow?.(r.entity);
  return (
    <div className={`chg-record${r.dropped ? ' dropped' : ''}${record === r.entity ? ' selected' : ''}`}>
      <div className="chg-head">
        <span className="chg-kind">record</span>
        <button type="button" className="link" onClick={() => onRecord(record === r.entity ? undefined : r.entity)} title={r.entity}>
          {r.label}
        </button>
        {r.dropped ? <span className="badge outcome-no_output">dropped</span> : <span className="muted">{r.changes.length} change(s)</span>}
        {follow && (
          <button type="button" className="link follow" onClick={follow} title="Follow this record through the whole run">
            Follow ⇢
          </button>
        )}
      </div>
      {!r.dropped && <EntityBlock e={r} all={all} />}
      {r.dropped && all && r.unchanged.length > 0 && (
        <ul className="chg-list">
          {r.unchanged.map((a) => (
            <li key={a.key} className="chg chg-removed">
              <span className="chg-mark">−</span>
              <code className="chg-path">attributes["{a.key}"]</code>
              <code className="val before">{a.value}</code>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * GroupedChanges leads with a field-level overview of a recorded diff and
 * keeps the resource/scope/record hierarchy as an alternate detail view.
 */
interface Props {
  diff?: Diff;
  before: unknown;
  after?: unknown;
  record?: string;
  onRecord(e: string | undefined): void;
  onFollow?(entity: string): (() => void) | undefined;
  onCreatedFollow?(entity: string): (() => void) | undefined;
}

export function GroupedChanges(props: Props) {
  const [mode, setMode] = useState<'fields' | 'records'>('fields');
  const overview = useMemo(() => changeOverview(props.diff, props.before, props.after, props.record), [props.diff, props.before, props.after, props.record]);
  const scopedDiff = useMemo(() => props.diff && ({
    ...props.diff,
    changes: overview.fields.flatMap((f) => f.edits.map((e) => e.change)),
    removed: props.diff.removed?.filter((r) => overview.records.some((x) => x.kind === 'dropped' && x.entity === refEntity(r))),
    added: props.diff.added?.filter((r) => overview.records.some((x) => x.kind === 'created' && x.entity === refEntity(r))),
    unresolved: undefined,
  }), [props.diff, overview]);
  if (!props.diff) return <p className="muted">No diff for this input. The stage may have buffered it; use Data or Next to inspect its emission.</p>;
  return <div className="grouped">
    <ChangesOverview {...props} diff={props.diff} overview={overview} mode={mode} onMode={setMode} />
    {mode === 'records' && <ResourceChanges {...props} diff={scopedDiff} />}
  </div>;
}

function ResourceChanges({
  diff,
  before,
  record,
  onRecord,
  onFollow,
}: Props) {
  const [all, setAll] = useState(false);
  const g = useMemo(() => groupChanges(diff, before), [diff, before]);
  if (!diff) return <p className="muted">No diff for this input.</p>;
  return (
    <div className="grouped">
      <div className="chg-summary">
        <label className="chg-all">
          <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> show unchanged attributes
        </label>
      </div>
      {g.resources.map((r) => (
        <section key={r.index} className="chg-resource">
          <div className="chg-head">
            <span className="chg-kind">resource</span>
            <strong>{r.index < 0 ? 'unplaced records' : r.label}</strong>
            {r.resource ? <span className="muted">{r.resource.changes.length} change(s)</span> : <span className="muted">unchanged</span>}
          </div>
          {r.resource && <EntityBlock e={r.resource} all={all} />}
          {r.scopes.map((sc) => (
            <div key={sc.index} className="chg-scope">
              {sc.scope && (
                <>
                  <div className="chg-head">
                    <span className="chg-kind">scope</span>
                    <span>{sc.scope.label}</span>
                  </div>
                  <EntityBlock e={sc.scope} all={all} />
                </>
              )}
              {sc.records.map((rec) => (
                <RecordBlock key={rec.entity} r={rec} all={all} record={record} onRecord={onRecord} onFollow={onFollow} />
              ))}
            </div>
          ))}
        </section>
      ))}
      {g.created.length > 0 && (
        <section className="chg-resource created">
          <div className="chg-head">
            <span className="chg-kind">created</span>
            <span className="muted">records in the output that match nothing in the input</span>
          </div>
          <ul className="chg-list">
            {g.created.map((r, i) => (
              <li key={i} className="chg chg-added">
                <span className="chg-mark">★</span>
                <span>{r.name || refEntity(r)}</span> <code className="muted">{refEntity(r)}</code>
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}
