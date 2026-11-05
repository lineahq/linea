import { describe, expect, it } from 'vitest';
import type { EvaluationInfo } from '../api/types';
import { evaluationRows } from './evaluations';

const info = (id: string, created: string, outcome: string, client = 'mcp'): EvaluationInfo => ({ id, created, outcome, client, run_id: 'r1' });

describe('recorded checks', () => {
  it('lists the newest first and names who recorded each', () => {
    const rows = evaluationRows([info('e1', '2026-10-24T10:00:00Z', 'pass'), info('e2', '2026-10-24T11:00:00Z', 'error', 'cli')]);
    expect(rows.map((r) => r.id)).toEqual(['e2', 'e1']);
    expect(rows[0]).toMatchObject({ outcome: 'error', tone: 'fail', icon: 'circle-x', by: 'the CLI' });
    expect(rows[1]).toMatchObject({ tone: 'pass', by: 'an MCP client' });
  });

  it('keeps an unknown client and outcome as given', () => {
    expect(evaluationRows([info('e1', 'x', 'odd', 'robot')])[0]).toMatchObject({ by: 'robot', icon: 'circle-help', tone: 'odd' });
    expect(evaluationRows(undefined)).toEqual([]);
  });
});
