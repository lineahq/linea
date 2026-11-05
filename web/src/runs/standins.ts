import type { Report } from '../api/types';

export interface StandInLine {
  /** What stood in: a component reference or a ${scheme:target} reference. */
  subject: string;
  text: string;
}

export interface StandIns {
  components: StandInLine[];
  references: StandInLine[];
}

const REMEDIES: Record<string, string> = {
  missing: 'the file does not exist on the server host; add it to the scenario’s mocks.files to supply its content',
  unreadable: 'the file could not be read; add it to the scenario’s mocks.files to supply its content',
  outside_roots: 'the server reads no host file; add it to the scenario’s mocks.files to supply its content',
  unbound: 'declare it under the scenario’s environment; the server reads no host environment variable',
  unsupported_scheme: 'the debugger has no provider for this scheme; nothing is fetched',
};

/**
 * standIns lists what ran in place of the real thing:
 * components whose type is not in the distribution, components configured
 * with stand-in values, and references that were unavailable. The overview
 * shows them in the open; checks on data that passed them are inconclusive.
 */
export function standIns(rep?: Report): StandIns {
  const out: StandIns = { components: [], references: [] };
  for (const c of rep?.topology?.components ?? []) {
    if (c.mode === 'stand_in') {
      out.components.push({ subject: c.ref, text: `ran as a stand-in: ${c.note ?? 'its type is not in the distribution'}.` });
    } else if (c.stand_in_values?.length) {
      out.components.push({ subject: c.ref, text: `ran with stand-in values for ${c.stand_in_values.map((v) => '${' + v + '}').join(', ')}.` });
    }
  }
  for (const r of rep?.references ?? []) {
    if (r.source !== 'stand_in') continue;
    const remedy = REMEDIES[r.reason ?? ''];
    out.references.push({
      subject: '${' + `${r.scheme}:${r.target}` + '}',
      text: `was unavailable (${r.reason ?? 'unknown'}); the stand-in "${r.stand_in ?? ''}" was used${remedy ? `. ${remedy[0].toUpperCase()}${remedy.slice(1)}.` : '.'}`,
    });
  }
  return out;
}
