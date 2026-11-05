import type { CaptureDeriveRequest, CaptureFixture } from '../api/types';
import { readScenario } from '../scenario/scenarioDoc';
import type { StreamSelection } from './recordView';

/**
 * Turning chosen capture records into scenario inputs, outside any component.
 * The component supplies the destination (configuration and setup text as the
 * user's draft has them) and applies the result in one draft update.
 */

/** The platform's bound on the fixtures materialised for one scenario edit. */
export const MAX_FIXTURE_BYTES = 4 * 1024 * 1024;

export interface MaterializeOptions {
  sessionId: string;
  groups: StreamSelection[];
  configYAML: string;
  setupYAML: string;
  signal: AbortSignal;
  fetchFixture: (sessionId: string, request: CaptureDeriveRequest, signal: AbortSignal) => Promise<CaptureFixture>;
  onProgress?: (done: number, total: number) => void;
}

/**
 * materializeFixtures asks the platform for one fixture per source stream,
 * sequentially, and refuses once their payloads exceed MAX_FIXTURE_BYTES. It
 * stops at the first failure and writes nothing.
 */
export async function materializeFixtures(o: MaterializeOptions): Promise<CaptureFixture[]> {
  const fixtures: CaptureFixture[] = [];
  let bytes = 0;
  for (const [index, group] of o.groups.entries()) {
    o.onProgress?.(index, o.groups.length);
    if (!group.receiver) throw new Error('Choose a receiver for every selected stream.');
    const result = await o.fetchFixture(
      o.sessionId,
      {
        selection: { stream: group.stream, sequences: group.sequences, watermark: group.watermark, receiver: group.receiver },
        config_yaml: o.configYAML,
        setup_yaml: o.setupYAML,
      },
      o.signal,
    );
    bytes += new TextEncoder().encode(result.fixture_json).length;
    if (bytes > MAX_FIXTURE_BYTES) throw new Error('The selected inputs exceed 4 MiB. Select fewer records and try again.');
    fixtures.push(result);
  }
  o.signal.throwIfAborted();
  return fixtures;
}

/** draftDrifted: the destination changed while fixtures were being prepared, so the receivers must be reviewed again. */
export function draftDrifted(latest: { configYAML: string; setup: string }, prepared: { configYAML: string; setup: string }): boolean {
  return latest.configYAML !== prepared.configYAML || latest.setup !== prepared.setup;
}

export interface DraftSummary {
  inputs: number;
  mocks: number;
  assertions: number;
}

/** draftSummary counts what an append keeps; a draft that does not parse has an error instead. */
export function draftSummary(scenarioYAML: string): DraftSummary | { error: string } {
  const read = readScenario(scenarioYAML.trim() ? scenarioYAML : 'version: 1\ninputs: []\n');
  if ('error' in read) return read;
  const m = read.model;
  return { inputs: m.inputs.length, mocks: m.files.length + m.kubeObjects.length + m.kubeFiles.length + m.providers.length + m.environment.length, assertions: m.assertions.length };
}

const count = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`;

/** keepsText: "Existing 1 input, 2 mocks and 2 assertions are kept." */
export function keepsText(s: DraftSummary): string {
  if (!s.inputs && !s.mocks && !s.assertions) return 'The scenario is empty, so nothing existing is affected.';
  return `Existing ${count(s.inputs, 'input')}, ${count(s.mocks, 'mock')} and ${count(s.assertions, 'assertion')} are kept.`;
}
