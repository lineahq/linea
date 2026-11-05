import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import type { OTTLEvaluation } from '../api/types';
import { checkInput, evaluatedOn, EXAMPLES, exampleText, playgroundConfig, processorYAML, recordSummary, replacesWork, statementLines, statementResults } from './playground';

describe('OTTL playground', () => {
  it('names the signal of an OTLP JSON payload', () => {
    expect(checkInput('{"resourceSpans": []}')).toMatchObject({ ok: true, signal: 'traces' });
    expect(checkInput('{"resourceMetrics": []}')).toMatchObject({ ok: true, signal: 'metrics' });
    for (const text of ['', '{', '[]', '{"resourceLogs": [], "resourceSpans": []}', '{"logs": []}']) {
      expect(checkInput(text).ok, text).toBe(false);
    }
  });

  it('reads one statement per line, skipping blanks and comments', () => {
    expect(statementLines('# note\n\n  set(a, 1)  \r\nset(b, 2)\n')).toEqual([
      { line: 3, text: 'set(a, 1)' },
      { line: 4, text: 'set(b, 2)' },
    ]);
  });

  it('writes a configuration that keeps every statement byte for byte', () => {
    const statements = ['set(log.attributes["a"], "x: y") where log.body == "#1"', 'replace_pattern(log.body, "\\\\d+", "*")', "set(log.body, 'é\\n')"];
    const cfg = parse(playgroundConfig('logs', statements, 'ignore'));
    expect(cfg.processors['transform/playground']).toEqual({ error_mode: 'ignore', log_statements: statements });
    expect(cfg.service.pipelines).toEqual({ logs: { receivers: ['otlp'], processors: ['transform/playground'], exporters: ['debug'] } });
    expect(parse(processorYAML('metrics', ['set(metric.unit, "s")'], 'propagate'))).toEqual({
      'transform/playground': { error_mode: 'propagate', metric_statements: ['set(metric.unit, "s")'] },
    });
  });

  it('pairs each line with its statement in the replay', () => {
    const ev = {
      valid: true,
      diagnostics: [],
      replay: { groups: [{ index: 0, statements: [
        { index: 0, text: 'a', matched: 2, not_matched: 1, changes: [{}, {}] },
        { index: 1, text: 'b', matched: 0, not_matched: 3, matched_without_change: 0, errors: [{ phase: 'execute', entity: 'log[0]', message: 'bad' }] },
      ] }] },
    } as unknown as OTTLEvaluation;
    expect(statementResults([{ line: 2, text: 'a' }, { line: 5, text: 'b' }], ev)).toEqual([
      { key: 'S0', line: 2, text: 'a', matched: 2, notMatched: 1, unchanged: 0, changes: 2, errors: [] },
      { key: 'S1', line: 5, text: 'b', matched: 0, notMatched: 3, unchanged: 0, changes: 0, errors: [{ entity: 'log[0]', message: 'bad' }] },
    ]);
    expect(statementResults([{ line: 1, text: 'a' }], undefined)).toEqual([]);
  });

  it('says what happened to the input records', () => {
    expect(recordSummary('logs', ['edited', 'unchanged', 'edited'])).toBe('3 log records: 2 changed, 1 unchanged');
    expect(recordSummary('traces', ['dropped', 'new'])).toBe('1 span: 1 dropped, 1 new');
    expect(recordSummary('metrics', [])).toBe('0 data points');
  });

  it('has a valid example for each signal', () => {
    expect(EXAMPLES.map((e) => e.signal)).toEqual(['logs', 'traces', 'metrics']);
    for (const e of EXAMPLES) {
      expect(checkInput(exampleText(e))).toMatchObject({ ok: true, signal: e.signal });
      expect(statementLines(e.statements).length).toBeGreaterThan(0);
    }
  });

  it('asks before an example replaces what the user wrote', () => {
    const [logs, traces] = EXAMPLES;
    expect(replacesWork('', '')).toBe(false);
    expect(replacesWork(exampleText(logs), traces.statements)).toBe(false);
    expect(replacesWork(exampleText(logs), 'set(log.body, "mine")')).toBe(true);
    expect(replacesWork('{"resourceLogs": []}', '')).toBe(true);
  });
});

describe('evaluatedOn', () => {
  it('names the runtime that answered, which may be the team default', () => {
    expect(evaluatedOn({ kind: 'built_in', collector_version: '0.162.0', verified: true }, undefined)).toEqual({
      text: 'Evaluated on Built-in · Collector 0.162.0, without a run. A preview, never a test.',
      unverified: false,
    });
    expect(evaluatedOn({ kind: 'container', name: 'v0163', collector_version: '0.163.0', verified: false }, '').unverified).toBe(true);
    expect(evaluatedOn({ kind: 'container', name: 'v0163', collector_version: '0.163.0', verified: false }, 'other').text)
      .toBe('Evaluated on v0163 · Collector 0.163.0, without a run. A preview, never a test.');
  });

  it('falls back to the selected runtime, or the default one, when the answer names none', () => {
    expect(evaluatedOn(undefined, 'mine')).toEqual({ text: 'Evaluated on runtime mine, without a run. A preview, never a test.', unverified: false });
    expect(evaluatedOn(undefined, undefined).text).toBe('Evaluated on the default runtime, without a run. A preview, never a test.');
  });
});
