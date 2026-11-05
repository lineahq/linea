import { isMap, isSeq, parseDocument } from 'yaml';
import type { CaptureFixture } from '../api/types';

export function appendCaptureInput(text: string, fixture: CaptureFixture): string {
  return appendCaptureInputs(text, [fixture]);
}

/** Build one draft update for all streams, preserving existing YAML and edits. */
export function appendCaptureInputs(text: string, fixtures: CaptureFixture[]): string {
  if (!fixtures.length) return text;
  const doc = parseDocument(text.trim() ? text : 'version: 1\ninputs: []\n');
  if (doc.errors.length || !isMap(doc.contents)) throw new Error('The open scenario must be valid YAML before adding captured input.');
  let inputs = doc.get('inputs', true);
  if (inputs === undefined) { doc.set('inputs', doc.createNode([])); inputs = doc.get('inputs', true); }
  if (!isSeq(inputs)) throw new Error('Scenario inputs must be a list.');
  let origins = doc.get('capture_origins', true);
  if (origins === undefined) { doc.set('capture_origins', doc.createNode([])); origins = doc.get('capture_origins', true); }
  if (!isSeq(origins)) throw new Error('Capture origins must be a list.');
  if (origins.items.length + fixtures.length > 64) throw new Error('A scenario supports at most 64 captured inputs. Select fewer streams or start another scenario.');
  for (const fixture of fixtures) {
    inputs.add(doc.createNode({ receiver: fixture.receiver, signal: fixture.signal, data: fixture.fixture_json }));
    origins.add(doc.createNode(fixture.derivation));
  }
  if (new TextEncoder().encode(JSON.stringify(origins.toJSON())).length > 128 * 1024) throw new Error('Capture provenance exceeds 128 KiB; use a separate scenario document.');
  if (!doc.has('version')) doc.set('version', 1);
  return doc.toString();
}
