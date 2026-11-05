import { describe, expect, it } from 'vitest';
import type { RunInputs } from '../api/types';
import { draftDifferences, draftsEmpty, differenceSummary, lineChanges } from './draftChanges';

const src = (text: string) => ({ text, sha256: 'x' });
const payload = '{"resourceLogs":[]}';
const quickDoc = (data = payload) => `version: 1\ninputs:\n  - receiver: otlp\n    data: '${data}'\n`;
const quickRun: RunInputs = { run_id: 'r', mode: 'quick', config: src('a: 1\nb: 2\n'), fixture: src(payload), receiver: 'otlp' };
const scenarioText = 'version: 1\ninputs:\n  - {receiver: otlp, file: a.json}\n';
const scenarioRun: RunInputs = { run_id: 'r', mode: 'scenario', config: src('a: 1\n'), scenario: src(scenarioText) };

describe('lineChanges', () => {
  it('lists added and removed lines and ignores moves and blank lines', () => {
    expect(lineChanges('a\nb\nc\n', 'c\nb\n\nd\n')).toEqual({ added: ['d'], removed: ['a'] });
    expect(lineChanges('a\na\n', 'a\n')).toEqual({ added: [], removed: ['a'] });
  });
});

describe('draftDifferences', () => {
  it('finds nothing when the drafts are what the run used', () => {
    expect(draftDifferences({ configYAML: 'a: 1\nb: 2\n', scenarioYAML: quickDoc() }, quickRun)).toEqual([]);
    expect(draftDifferences({ configYAML: 'a: 1', scenarioYAML: scenarioText }, scenarioRun)).toEqual([]);
  });

  it('does not call an empty draft a change', () => {
    expect(draftDifferences({ configYAML: '', scenarioYAML: '' }, quickRun)).toEqual([]);
  });

  it('finds an edited configuration', () => {
    const d = draftDifferences({ configYAML: 'a: 1\nb: 3\n', scenarioYAML: quickDoc() }, quickRun);
    expect(d).toHaveLength(1);
    expect(d[0]).toMatchObject({ part: 'configuration', added: ['b: 3'], removed: ['b: 2'] });
    expect(differenceSummary(d)).toBe('1 edit: configuration (2 lines).');
  });

  it('finds an edited payload of a quick run', () => {
    const d = draftDifferences({ configYAML: 'a: 1\nb: 2\n', scenarioYAML: quickDoc('{"resourceLogs":[{}]}') }, quickRun);
    expect(d.map((x) => x.part)).toEqual(['inputs']);
  });

  it('finds a quick run that now runs as a scenario', () => {
    const grown = quickDoc() + 'assertions:\n  - count: {exporter: debug, equals: 1}\n';
    expect(draftDifferences({ configYAML: 'a: 1\nb: 2\n', scenarioYAML: grown }, quickRun).map((x) => x.part)).toEqual(['inputs']);
  });

  it('compares the scenario text of a scenario run', () => {
    const edited = scenarioText + '  - {receiver: otlp, file: b.json}\n';
    const d = draftDifferences({ configYAML: 'a: 1', scenarioYAML: edited }, scenarioRun);
    expect(d).toHaveLength(1);
    expect(d[0].added).toEqual(['  - {receiver: otlp, file: b.json}']);
    expect(differenceSummary([...d, ...d])).toContain('2 edits');
  });
});

describe('draftsEmpty', () => {
  it('needs both drafts blank', () => {
    expect(draftsEmpty({ configYAML: ' ', scenarioYAML: '' })).toBe(true);
    expect(draftsEmpty({ configYAML: 'a', scenarioYAML: '' })).toBe(false);
  });
});
