import { describe, expect, it, vi } from 'vitest';
import type { CaptureDeriveRequest, CaptureFixture } from '../api/types';
import { draftDrifted, draftSummary, keepsText, materializeFixtures, MAX_FIXTURE_BYTES } from './addInputs';
import type { StreamSelection } from './recordView';

const group = (id: string, receiver = 'otlp'): StreamSelection => ({
  stream: { collector_id: id, pipeline: 'logs', point: 'egress', signal: 'logs' },
  sequences: [2, 1],
  watermark: 7,
  receiver,
});
const fixture = (json: string): CaptureFixture => ({ fixture_json: json, receiver: 'otlp', signal: 'logs', setup_yaml: '', derivation: {} as CaptureFixture['derivation'] });
const base = { sessionId: 's1', configYAML: 'cfg', setupYAML: 'setup', signal: new AbortController().signal };

describe('materializeFixtures', () => {
  it('requests one fixture per stream, in order, with the destination as the user has it', async () => {
    const fetchFixture = vi.fn(async (_id: string, _request: CaptureDeriveRequest, _signal: AbortSignal) => fixture('{}'));
    const progress: [number, number][] = [];
    const out = await materializeFixtures({ ...base, groups: [group('a'), group('b', 'otlp/2')], fetchFixture, onProgress: (d, t) => progress.push([d, t]) });
    expect(out).toHaveLength(2);
    expect(fetchFixture).toHaveBeenNthCalledWith(1, 's1', { selection: { stream: group('a').stream, sequences: [2, 1], watermark: 7, receiver: 'otlp' }, config_yaml: 'cfg', setup_yaml: 'setup' }, base.signal);
    expect(fetchFixture.mock.calls[1][1].selection.receiver).toBe('otlp/2');
    expect(progress).toEqual([[0, 2], [1, 2]]);
  });

  it('refuses a stream without a receiver before asking the platform', async () => {
    const fetchFixture = vi.fn();
    await expect(materializeFixtures({ ...base, groups: [group('a', '')], fetchFixture })).rejects.toThrow('Choose a receiver');
    expect(fetchFixture).not.toHaveBeenCalled();
  });

  it('stops once the payloads together exceed 4 MiB', async () => {
    const big = 'x'.repeat(MAX_FIXTURE_BYTES / 2 + 1);
    const fetchFixture = vi.fn(async () => fixture(big));
    await expect(materializeFixtures({ ...base, groups: [group('a'), group('b'), group('c')], fetchFixture })).rejects.toThrow('exceed 4 MiB');
    expect(fetchFixture).toHaveBeenCalledTimes(2);
  });

  it('propagates a platform failure and an abort', async () => {
    await expect(materializeFixtures({ ...base, groups: [group('a')], fetchFixture: async () => { throw new Error('capture expired'); } })).rejects.toThrow('capture expired');
    const controller = new AbortController();
    const fetchFixture = vi.fn(async () => {
      controller.abort();
      return fixture('{}');
    });
    await expect(materializeFixtures({ ...base, signal: controller.signal, groups: [group('a')], fetchFixture })).rejects.toThrow();
  });
});

describe('draftDrifted', () => {
  it('notices a configuration or setup change', () => {
    const prepared = { configYAML: 'a', setup: 'b' };
    expect(draftDrifted({ configYAML: 'a', setup: 'b' }, prepared)).toBe(false);
    expect(draftDrifted({ configYAML: 'a2', setup: 'b' }, prepared)).toBe(true);
    expect(draftDrifted({ configYAML: 'a', setup: 'b2' }, prepared)).toBe(true);
  });
});

describe('draftSummary', () => {
  const doc = `version: 1
inputs:
  - receiver: otlp
    signal: logs
    data: "{}"
assertions:
  - name: a
    target: {exporter: debug}
    conditions: {}
  - name: b
    target: {exporter: debug}
mocks:
  files:
    - path: /var/log/a.log
      content: x
  providers:
    vault: {}
`;
  it('counts inputs, mocks and assertions', () => {
    expect(draftSummary(doc)).toEqual({ inputs: 1, mocks: 2, assertions: 2 });
  });
  it('treats an empty draft as an empty scenario', () => {
    expect(draftSummary('  ')).toEqual({ inputs: 0, mocks: 0, assertions: 0 });
  });
  it('reports a draft that does not parse', () => {
    expect(draftSummary('inputs: [')).toHaveProperty('error');
  });
  it('says what an append keeps', () => {
    expect(keepsText({ inputs: 1, mocks: 2, assertions: 2 })).toBe('Existing 1 input, 2 mocks and 2 assertions are kept.');
    expect(keepsText({ inputs: 0, mocks: 0, assertions: 0 })).toMatch(/empty/);
  });
});
