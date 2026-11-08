import { isMap, isSeq, parseDocument } from 'yaml';
import type { CaptureFixture } from '../api/types';

export function appendCaptureInput(text: string, fixture: CaptureFixture): string {
  return appendCaptureInputs(text, [fixture]);
}

/**
 * mergeFixtures makes one input per signal and receiver: the fixtures of
 * several streams of one signal become one OTLP document whose resource
 * lists are concatenated in selection order. A lone fixture keeps its exact
 * bytes. Each stream's derivation stays in `derivations`.
 */
export function mergeFixtures(fixtures: CaptureFixture[]): { fixture: CaptureFixture; derivations: CaptureFixture['derivation'][] }[] {
  const groups = new Map<string, CaptureFixture[]>();
  for (const f of fixtures) {
    const key = `${f.signal}\u0000${f.receiver}`;
    groups.set(key, [...(groups.get(key) ?? []), f]);
  }
  return [...groups.values()].map((group) => {
    const derivations = group.map((f) => f.derivation);
    if (group.length === 1) return { fixture: group[0], derivations };
    const merged: Record<string, unknown> = {};
    for (const f of group) {
      const doc = JSON.parse(f.fixture_json) as Record<string, unknown>;
      for (const [k, v] of Object.entries(doc)) {
        if (Array.isArray(v)) merged[k] = [...((merged[k] as unknown[] | undefined) ?? []), ...v];
        else if (!(k in merged)) merged[k] = v;
      }
    }
    return { fixture: { ...group[0], fixture_json: JSON.stringify(merged), payload: merged }, derivations };
  });
}

/**
 * Build one draft update for all streams, preserving existing YAML and edits.
 * Streams of one signal going to one receiver become one input.
 */
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
  if (origins.items.length + fixtures.length > 64) throw new Error('A scenario supports at most 64 captured streams. Select fewer streams or start another scenario.');
  for (const { fixture, derivations } of mergeFixtures(fixtures)) {
    inputs.add(doc.createNode({ receiver: fixture.receiver, signal: fixture.signal, data: fixture.fixture_json }));
    for (const d of derivations) origins.add(doc.createNode(d));
  }
  if (new TextEncoder().encode(JSON.stringify(origins.toJSON())).length > 128 * 1024) throw new Error('Capture provenance exceeds 128 KiB; use a separate scenario document.');
  if (!doc.has('version')) doc.set('version', 1);
  return doc.toString();
}
