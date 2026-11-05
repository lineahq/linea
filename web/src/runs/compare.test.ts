import { describe, expect, it } from 'vitest';
import type { Run } from '../api/types';
import { different } from './compareFixture';
import { COMPARE_FILTERS, assertionOutcomes, compareCandidates, compareItems, comparePair, exporterText, fateText, headline, outputRows, pathLevel, summaryCards, valueText, verdict } from './compare';

describe('choosing the runs to compare', () => {
  const run = (id: string, created: string, state = 'done') => ({ id, created, state }) as Run;
  // The run list is newest admitted first.
  const runs = [run('c', '2026-10-13T12:00:00Z', 'error'), run('b', '2026-10-13T11:00:00Z', 'running'), run('a', '2026-10-13T10:00:00Z'), run('d', '2026-10-13T09:00:00Z', 'cancelled')];
  it('offers the other finished runs in list order', () => {
    expect(compareCandidates(runs, 'a').map((r) => r.id)).toEqual(['c', 'd']);
  });
  it('makes the earlier admitted run the before run whichever is selected, even at the same time', () => {
    expect(comparePair(runs, 'c', 'a')).toEqual({ before: 'a', after: 'c' });
    expect(comparePair(runs, 'a', 'c')).toEqual({ before: 'a', after: 'c' });
    // A configuration comparison admits both runs at once.
    const pair = [run('after', '2026-10-13T11:15:46.6019386Z'), run('before', '2026-10-13T11:15:46.6019386Z')];
    expect(comparePair(pair, 'after', 'before')).toEqual({ before: 'before', after: 'after' });
    expect(comparePair(runs, 'a', 'gone')).toBeUndefined();
  });
});

describe('the comparison header', () => {
  it('states the outcome in one sentence', () => {
    expect(headline(different)).toEqual({ text: 'Different: 1 of 2 exporter outputs differ; 2 injected records changed.', cls: 'bad' });
    expect(headline({ ...different, outcome: 'same', exporters: [different.exporters![1]] })).toEqual({ text: 'Same: every exporter received the same records (1 output).', cls: 'good' });
    expect(headline({ ...different, outcome: 'same', exporters: [] }).text).toBe('Same: neither run delivered any output.');
    const inconclusive = headline({ ...different, outcome: 'inconclusive', reasons: [{ code: 'after_truncated', message: 'The after run’s evidence was truncated.' }] });
    expect(inconclusive).toEqual({ text: 'Inconclusive: 1 reason below prevent a verdict.', cls: 'warn' });
  });
  it('says how each exporter compares, never from counts alone', () => {
    const e = { exporter: 'x', signal: 'logs', before: 2, after: 2, equal: false };
    expect(exporterText(e)).toBe('different records, same count');
    expect(exporterText({ ...e, after: 3 })).toBe('different records');
    expect(exporterText({ ...e, equal: true })).toBe('same records');
    expect(exporterText({ ...e, equal: true, note: 'order' })).toBe('same records, different order');
    expect(exporterText({ ...e, equal_after_ignore: true })).toBe('same apart from ignored paths');
    expect(exporterText({ ...e, only_in: 'after' })).toBe('output only in the after run');
  });
});

describe('changed records', () => {
  const items = compareItems(different);
  it('makes one row per changed record that says how it differs without repeating fields', () => {
    expect(items.map((i) => [i.id, i.title, i.why, i.tone])).toEqual([
      ['1:resource[0].scope[0].log_record[0]', 'login retry', 'Changed at 1 exporter', 'mod'],
      ['1:resource[0].scope[0].log_record[2]', 'Record resource[0].scope[0].log_record[2] of input 1', 'Ends differently', 'drop'],
    ]);
    expect(COMPARE_FILTERS.map((f) => items.filter(f.match).length)).toEqual([2, 1, 1]);
  });
  it('groups field changes per exporter by level, keeping types and empty strings', () => {
    expect(items[0].exporters).toEqual([{ exporter: 'otlp/backend (logs)', fields: [
      { level: 'record', path: 'log.severity_text', op: 'modified', before: '"WARNING"', after: '"warning"', type: 'Str', confidence: undefined },
      { level: 'resource', path: 'resource.attributes["env"]', op: 'type_changed', before: '1', after: '"1"', type: 'Int → Str', confidence: undefined },
      { level: 'record', path: 'log.attributes["note"]', op: 'added', before: undefined, after: '""', type: 'Str', confidence: undefined },
    ] }]);
    expect(fateText(items[1].record.after)).toBe('dropped at filter/health (logs)');
    expect(fateText(items[1].record.before)).toBe('delivered to otlp/backend (logs)');
    expect(fateText({ lineage_id: '' })).toBe('no end recorded');
  });
  it('places paths and marks cut values', () => {
    expect(['resource.attributes["a"]', 'scope.name', 'instrumentation_scope.version', 'span.name', 'datapoint.value_int'].map(pathLevel))
      .toEqual(['resource', 'scope', 'scope', 'record', 'record']);
    expect(valueText({ value: 'abc', type: 'Str', truncated: true })).toBe('"abc"…');
    expect(valueText(undefined)).toBeUndefined();
  });
  it('has no rows when the inputs are not proven identical', () => {
    expect(compareItems({ ...different, records: undefined, inputs_identical: false })).toEqual([]);
  });
});

describe('the comparison layout', () => {
  it('opens with eligibility: comparable, or every reason it is not', () => {
    expect(verdict(different)).toEqual({
      title: 'Comparable', tone: 'ok', reasons: [],
      lead: 'Same captured inputs (identical input digest). Different: 1 of 2 exporter outputs differ; 2 injected records changed.',
    });
    const inconclusive = {
      ...different, outcome: 'inconclusive', inputs_identical: true,
      reasons: [{ code: 'before_incomplete', message: 'the before run is incomplete (deadline)' }, { code: 'builds_differ', message: 'the runs used different builds' }],
    };
    expect(verdict(inconclusive)).toEqual({
      title: 'Inconclusive', tone: 'no',
      lead: 'Differences between these executions would not prove anything about the configuration:',
      reasons: ['The before run is incomplete (deadline)', 'The runs used different builds'],
    });
    expect(verdict({ ...inconclusive, inputs_identical: false, records: undefined }).title).toBe('Not comparable');
    expect(verdict({ ...inconclusive, reasons: [] }).lead).toBe('The platform reported no verdict and gave no reason.');
  });
  it('counts what differs from the platform tallies and names the records it could not follow', () => {
    expect(summaryCards(different)).toEqual([
      { id: 'outputs', label: 'Outputs changed', value: 1, of: 'of 2 exporter outputs' },
      { id: 'records', label: 'Records changed', value: 2, of: 'of 4 aligned records', note: '1 not followed with confidence' },
      { id: 'routes', label: 'Routes changed', value: 0, of: 'of 0 routes' },
    ]);
    expect(summaryCards({ ...different, records: undefined, inputs_identical: false }).map((c) => c.id)).toEqual(['outputs', 'routes']);
    const routed = { ...different, routes: [{ connector: 'routing', pipeline: 'logs/a', before: 1, after: 2, changed: true }, { connector: 'routing', pipeline: 'logs/b', before: 1, after: 1, changed: false }] };
    expect(summaryCards(routed)[2]).toMatchObject({ value: 1, of: 'of 2 routes' });
  });
  it('rows each exporter output with its count change and how its records compare', () => {
    expect(outputRows(different).map((r) => [r.exporter, r.before, r.after, r.delta, r.records])).toEqual([
      ['otlp/backend', 3, 2, { text: '−1 logs', tone: 'down' }, 'different records'],
      ['debug', 1, 1, { text: 'no change', tone: 'same' }, 'same records'],
    ]);
    const sameCount = outputRows({ exporters: [{ exporter: 'x', signal: 'logs', before: 2, after: 2, equal: false }] });
    expect(sameCount[0].delta).toEqual({ text: 'same count, different records', tone: 'down' });
    expect(outputRows({ exporters: [{ exporter: 'y', signal: 'traces', before: 0, after: 4, equal: false, only_in: 'after' }] })[0].delta).toEqual({ text: '+4 traces', tone: 'up' });
  });
  it('lists one side of the assertions', () => {
    const c = { assertions: [{ name: 'a', before: 'fail', after: 'pass', changed: true }, { name: 'b', after: 'pass', changed: false }] };
    expect(assertionOutcomes(c, 'before')).toEqual(['fail']);
    expect(assertionOutcomes(c, 'after')).toEqual(['pass', 'pass']);
    expect(assertionOutcomes({}, 'after')).toEqual([]);
  });
});
