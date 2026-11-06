import { useMemo } from 'react';
import type { Batching } from '../api/types';
import { RecordList, type RecordFilter } from './RecordList';
import { Pill, RecordRow, type GlyphTone } from './RecordRow';
import {
  batcherRows, batchRows, impactRows, itemNoun, metadataNote, settingsText, summaryText, timerNote, TRIGGER_LABELS,
  verificationText, type BatchRow, type Trigger,
} from './batching';

const TONES: Record<Trigger, GlyphTone> = { size: 'keep', timeout: 'mod', shutdown: 'none', immediate: 'none' };
const FILTERS: [Trigger | 'all', string][] = [['all', 'All'], ['size', 'Size reached'], ['timeout', 'Timer'], ['shutdown', 'Shutdown'], ['immediate', 'Right away']];
const filters: RecordFilter<BatchRow>[] = FILTERS.map(([id, label]) => ({ id, label, match: (r) => id === 'all' || r.trigger === id }));

/**
 * BatchingPanel explains a batch processor: what batching changed between
 * the requests in and the batches out, the batchers metadata_keys keeps
 * apart, and each batch with why it left, in the shared record list.
 */
export function BatchingPanel({ batching, live }: { batching?: Batching; live?: boolean }) {
  const rows = useMemo(() => (batching ? batchRows(batching) : []), [batching]);
  if (!batching && live) return <div className="placeholder">Batches are explained when the run ends.</div>;
  if (!batching) return <div className="placeholder">No batches were recorded for this processor in this run.</div>;
  const b = batching;
  const batchers = batcherRows(b);
  const check = verificationText(b);
  const noun = itemNoun(b.signal, 2).replace(/^2 (.)/, (_m, c: string) => c.toUpperCase());
  return (
    <div className="rv-changes rv-padded">
      <p className="rv-notice rv-info">
        {summaryText(b)} <span className="mono bt-settings">{settingsText(b)}</span>
      </p>
      <h3 className="rv-section">Impact</h3>
      <div className="rv-scroll">
        <table className="grid bt-table" aria-label="What batching changed">
          <thead><tr><th scope="col" /><th scope="col" className="bt-num">Into batch</th><th scope="col" className="bt-num">Out of batch</th><th scope="col"><span className="sr-only">Meaning</span></th></tr></thead>
          <tbody>
            {impactRows(b).map((r) => (
              <tr key={r.label}><th scope="row">{r.label}</th><td className="bt-num">{r.before}</td><td className="bt-num">{r.after}</td><td className="bt-note">{r.note}</td></tr>
            ))}
          </tbody>
        </table>
      </div>
      <h3 className="rv-section">Batchers</h3>
      <p className="rv-foot">{metadataNote(b)}</p>
      {batchers.length > 0 && <div className="rv-scroll">
        <table className="grid bt-table" aria-label="Batchers">
          <thead><tr><th scope="col">Batcher</th><th scope="col" className="bt-num">Requests</th><th scope="col" className="bt-num">{noun}</th><th scope="col" className="bt-num">Batches</th><th scope="col">Why sent</th></tr></thead>
          <tbody>
            {batchers.map((x) => (
              <tr key={x.index} className={x.refused ? 'rv-dim' : undefined}>
                <th scope="row" className="mono">{x.label}</th>
                <td className="bt-num">{x.requests}</td>
                <td className="bt-num">{x.items}</td>
                <td className="bt-num">{x.batches}</td>
                <td>{x.refused ? 'refused: over metadata_cardinality_limit' : x.reasons.map(([t, n]) => `${n} ${TRIGGER_LABELS[t]}`).join(', ') || 'nothing sent'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>}
      {(b.notes ?? []).map((n, i) => <p key={i} className="rv-notice">{n}</p>)}
      <h3 className="rv-section">Batches</h3>
      <RecordList
        items={rows}
        itemId={(r) => String(r.seq)}
        filters={filters}
        label="Batches and why each left"
        hint="see what it carries"
        pageNoun="batches"
        empty={rows.length ? undefined : 'No batch left this processor.'}
        renderRow={(r, s) => (
          <RecordRow {...s} onToggle={s.toggle} glyph={String(r.n)} glyphTone={TONES[r.trigger]} title={`Batch ${r.n}: ${itemNoun(b.signal, r.items)}`}
            why={r.why}
            pills={<>
              <Pill tone={TONES[r.trigger] === 'none' ? undefined : TONES[r.trigger] as 'keep' | 'mod'}>{TRIGGER_LABELS[r.trigger]}</Pill>
              {r.batcher && <Pill>{r.batcher}</Pill>}
              <Pill>{r.contents.length || '?'} request{r.contents.length === 1 ? '' : 's'}</Pill>
              {r.waited && <Pill>{r.waited}</Pill>}
            </>} />
        )}
        renderDetail={(r) => (
          <>
            {r.contents.length > 0
              ? <table className="grid bt-table" aria-label="Contents">
                <thead><tr><th scope="col">From</th><th scope="col">Service</th><th scope="col" className="bt-num">{noun}</th></tr></thead>
                <tbody>{r.contents.map((c) => <tr key={c.request}><td>Request {c.request}</td><td>{c.source}</td><td className="bt-num">{c.items}</td></tr>)}</tbody>
              </table>
              : <p className="rv-foot">The run recorded no lineage for this batch, so its requests are not known.</p>}
            <p className="rv-foot">
              {r.blocks} resource block{r.blocks === 1 ? '' : 's'}{r.distinct < r.blocks && ` for ${r.distinct} distinct resource${r.distinct === 1 ? '' : 's'}: batching does not merge equal resources (groupbyattrs after batch does)`}.
            </p>
            {timerNote(b, r) && <p className="rv-foot">{timerNote(b, r)}</p>}
          </>
        )}
      />
      <p className={`rv-foot${check.tone === 'warn' ? ' rv-shared' : ''}`}>{check.text}</p>
    </div>
  );
}
