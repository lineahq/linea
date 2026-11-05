import { useMemo } from 'react';
import type { RecordLineage, Sampling } from '../api/types';
import type { WorkspaceSearch } from '../router';
import { RecordList, type RecordFilter } from './RecordList';
import { Pill, RecordRow, type GlyphTone } from './RecordRow';
import { decisionRows, decisionSummary, type DecisionRow, type Tone } from './sampling';

const GLYPHS: Record<Tone, [string, GlyphTone]> = { kept: ['✓', 'keep'], dropped: ['−', 'drop'], pending: ['?', 'mod'] };
const FILTERS: [Tone | 'all', string][] = [['all', 'All'], ['kept', 'Kept'], ['dropped', 'Dropped'], ['pending', 'Undecided']];
const filters: RecordFilter<DecisionRow>[] = FILTERS.map(([id, label]) => ({ id, label, match: (r) => id === 'all' || r.tone === id }));

/**
 * SamplingPanel explains a tail_sampling processor's decisions in the shared
 * record list: one row per trace that says which policy decided it, opening
 * to the ladder of policies with what each one read in the trace.
 */
export function SamplingPanel({ sampling, lineage, live, setSearch }: {
  sampling?: Sampling;
  /** The run is still in progress: decisions are explained once it ends. */
  live?: boolean;
  lineage?: RecordLineage[];
  setSearch(patch: Partial<WorkspaceSearch>): void;
}) {
  const rows = useMemo(() => (sampling ? decisionRows(sampling, lineage) : []), [sampling, lineage]);
  if (!sampling && live) return <div className="placeholder">Sampling decisions are explained when the run ends.</div>;
  if (!sampling) return <div className="placeholder">No sampling decisions were recorded for this processor in this run.</div>;
  const policies = new Map((sampling.policies ?? []).map((p) => [p.name, p]));
  return (
    <div className="rv-changes rv-padded">
      <p className="rv-notice rv-info">
        {decisionSummary(rows)}. tail_sampling decides per trace, about {sampling.decision_wait} (decision_wait) after the trace's
        first span arrives, from all the spans received until then. A trace is kept when any policy samples it, unless a drop policy matches.
      </p>
      {(sampling.notes ?? []).map((n, i) => <p key={i} className="rv-notice">{n}</p>)}
      <RecordList
        items={rows}
        itemId={(r) => r.trace.trace_id}
        filters={filters}
        label="Traces and the policy that decided each"
        hint="see which policy decided it"
        pageNoun="traces"
        defaultOpen
        empty={rows.length ? undefined : 'No traces reached this processor.'}
        renderRow={(r, s) => {
          const [glyph, tone] = GLYPHS[r.tone];
          return <RecordRow {...s} onToggle={s.toggle} glyph={glyph} glyphTone={tone} title={r.trace.name}
            why={r.reason.charAt(0).toUpperCase() + r.reason.slice(1)}
            pills={<>
              <Pill tone={tone === 'mod' ? 'mod' : tone === 'drop' ? 'drop' : 'keep'}>{r.label.toLowerCase()}</Pill>
              <Pill>{r.trace.spans} span{r.trace.spans === 1 ? '' : 's'} · {r.trace.duration}</Pill>
            </>} />;
        }}
        renderDetail={(r) => (
          <>
            {r.cells.length === 0
              ? <p className="rv-foot">No policy was evaluated on this trace.</p>
              : <ol className="rv-ladder" aria-label="Policies">
                {r.cells.map((c) => {
                  const p = policies.get(c.policy);
                  return (
                    <li key={c.policy} className={`rv-step rv-step-${c.outcome}${c.decisive ? ' rv-decisive' : ''}`}>
                      <span className="rv-step-name">{c.policy}{p && <span className="rv-type">{p.type}</span>}</span>
                      <span className="rv-step-result">{c.label}{c.decisive && <span className="rv-step-flag"> · decided</span>}</span>
                      <span className="rv-step-evidence">
                        {c.evidence}
                        {p && <span className="rv-step-rule">{p.rule}</span>}
                      </span>
                    </li>
                  );
                })}
              </ol>}
            {r.late && <p className="rv-foot">{r.late}</p>}
            {r.warning && <p className="rv-notice">{r.warning}</p>}
            <p className="rv-foot mono" title={r.trace.trace_id}>Trace {r.trace.trace_id}</p>
            {r.lineage && <div><button type="button" className="rv-follow" aria-label={`Follow record: ${r.trace.name}`}
              onClick={() => setSearch({ trace: r.lineage, tab: 'journey', view: undefined })}>Follow record <span aria-hidden="true">→</span></button></div>}
          </>
        )}
      />
      <p className="rv-foot">Each policy shows what it decides on this trace on its own, replayed on the spans the processor evaluated.</p>
    </div>
  );
}
