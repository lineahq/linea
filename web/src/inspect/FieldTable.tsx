import { useState } from 'react';
import { LEVELS, LEVEL_HELP, LEVEL_LABELS, type FieldView, type Level } from './recordViews';

interface Props {
  fields: FieldView[];
  /** Show unchanged fields too, so the whole record and every path can be seen. */
  showAll: boolean;
  /** Show the statement that made each change. */
  withBy?: boolean;
  /** One snapshot, no comparison: show a single Value column. */
  single?: boolean;
  /** How many records share the resource or scope edits shown. */
  shared?: Partial<Record<Level, number>>;
}

/** CopyPath copies an OTTL path; the clipboard can be refused, so it says so. */
export function CopyPath({ path }: { path: string }) {
  const [label, setLabel] = useState('Copy');
  const done = (text: string) => { setLabel(text); setTimeout(() => setLabel('Copy'), 1200); };
  return (
    <button type="button" className="rv-copy" aria-label={`Copy path ${path}`}
      onClick={() => { try { navigator.clipboard.writeText(path).then(() => done('Copied'), () => done('Not copied')); } catch { done('Not copied'); } }}>
      {label}
    </button>
  );
}

/**
 * FieldTable lists a record's fields grouped by where they live: resource,
 * scope and the record itself. Each field shows the path to use in OTTL,
 * its value type and its value before and after.
 */
export function FieldTable({ fields, showAll, withBy, single, shared }: Props) {
  const shown = fields.filter((f) => showAll || f.op !== 'same');
  if (!shown.length) return <p className="rv-foot">No fields changed.</p>;
  return <>{LEVELS.map((level) => {
    const rows = shown.filter((f) => f.level === level);
    if (!rows.length) return null;
    const sharedBy = shared?.[level] ?? 0;
    return (
      <div key={level} className="rv-group">
        <div className="rv-group-head">
          <b>{LEVEL_LABELS[level]}</b><span>{LEVEL_HELP[level]}</span>
          {sharedBy > 1 && rows.some((f) => f.op !== 'same') && <span className="rv-shared">edit shared by {sharedBy} records</span>}
        </div>
        <div className="rv-scroll">
          <table className="rv-fields">
            <caption className="sr-only">{LEVEL_LABELS[level]} fields</caption>
            <colgroup>
              <col className="rv-col-path" />
              {single ? <col className="rv-col-value" /> : <><col className="rv-col-value" /><col className="rv-col-value" /></>}
              {withBy && <col className="rv-col-by" />}
            </colgroup>
            <thead><tr><th scope="col">Path</th>{single ? <th scope="col">Value</th> : <><th scope="col">Before</th><th scope="col">After</th></>}{withBy && <th scope="col">By</th>}</tr></thead>
            <tbody>
              {rows.map((f) => (
                <tr key={f.path} className={`rv-field rv-field-${f.op}`}>
                  <td>
                    <span className="rv-path">{f.path}</span><CopyPath path={f.path} />
                    {f.type && <span className="rv-type">{f.type}</span>}
                    {f.confidence === 'medium' && <span className="rv-type" title="The records were paired by content, not by identity">medium-confidence match</span>}
                  </td>
                  {single ? <td>{f.before}</td> : <>
                    <td className={f.op === 'removed' || f.op === 'modified' || f.op === 'type_changed' ? 'rv-before' : undefined}>
                      {f.before === undefined ? <span className="rv-none" aria-label="Not present before">—</span> : f.before}
                    </td>
                    <td className={f.op === 'added' || f.op === 'modified' || f.op === 'type_changed' ? 'rv-after' : undefined}>
                      {f.op === 'same' ? <span className="rv-none">same</span> : f.after === undefined ? <span className="rv-none" aria-label="Not present after">—</span> : f.after}
                    </td></>}
                  {withBy && <td className="rv-by">{f.by ?? ''}</td>}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    );
  })}</>;
}
