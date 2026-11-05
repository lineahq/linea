import { expect, it } from 'vitest';
import { appendCaptureInput, appendCaptureInputs } from './inputs';
import type { CaptureFixture } from '../api/types';
import { parse } from 'yaml';
import { scenarioSetup, runFields } from '../input/quickInput';
const fixture = { receiver: 'otlp', signal: 'logs', fixture_json: '{"resourceLogs":[]}', setup_yaml: '', payload: { resourceLogs: [] }, derivation: { session_id: 'capture-1', selection: { stream: { collector_id: 'collector-1', pipeline: 'logs', point: 'egress', signal: 'logs' }, watermark: 1, sequences: [1], receiver: 'otlp' }, source_digest: 'a'.repeat(64), digest: 'b'.repeat(64), destination_config_digest: 'c'.repeat(64), records: 1 }, limitations: [] } as CaptureFixture;
it('appends without replacing comments, earlier inputs, mocks or assertions', () => {
  const text = '# edits\nversion: 1\ninputs:\n  - receiver: old\n    data: "old"\nmocks:\n  files: []\nassertions: []\n';
  const next = appendCaptureInput(text, fixture);
  expect(next).toContain('# edits'); expect(next).toContain('receiver: old'); expect(next).toContain('receiver: otlp'); expect(next).toContain('mocks:'); expect(next).toContain('assertions:');
});
it('refuses an invalid existing draft instead of replacing it', () => { expect(() => appendCaptureInput('inputs: [', fixture)).toThrow('valid YAML'); expect(() => appendCaptureInput('version: 1\ninputs: bad', fixture)).toThrow('must be a list'); });
it('carries capture provenance through unsaved quick setup and later scenario edits', () => {
  const next = appendCaptureInput('version: 1\ninputs: []\nenvironment: {DECLARED: kept}\n', fixture);
  expect(parse(next).capture_origins).toEqual([fixture.derivation]);
  expect(parse(scenarioSetup(next)).capture_origins).toEqual([fixture.derivation]);
  const fields = runFields({ scenarioYAML: next });
  expect('setup_yaml' in fields && parse(fields.setup_yaml!).capture_origins).toEqual([fixture.derivation]);
  const twice = appendCaptureInput(next, fixture);
  expect(parse(twice).capture_origins).toEqual([fixture.derivation, fixture.derivation]);
});

it('appends a multi-stream batch together with exact fixture bytes and prior assertions', () => {
  const metric = {...fixture, signal: 'metrics', receiver: 'otlp/metrics', fixture_json: '{"resourceMetrics":[]}', derivation: {...fixture.derivation, digest: 'd'.repeat(64)}};
  const next = appendCaptureInputs('# retained\nversion: 1\ninputs: []\nassertions: []\n', [fixture, metric]);
  const doc = parse(next); expect(doc.inputs.map((input: {data: string}) => input.data)).toEqual([fixture.fixture_json, metric.fixture_json]);
  expect(doc.capture_origins).toHaveLength(2); expect(next).toContain('# retained'); expect(doc.assertions).toEqual([]);
});
