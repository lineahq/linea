import { describe, expect, it } from 'vitest';
import type { RecommendationDiff } from '../api/types';
import { different, recommendationDiffs } from './compareFixture';
import { recommendationChanges, recommendationsSummary } from './recommendationsCompare';

describe('recommendationsSummary', () => {
  it('counts the three parts and leaves out the zero ones', () => {
    expect(recommendationsSummary({ resolved: 2, new: 1, persists: 3 })).toBe('2 resolved, 1 new, 3 persist');
    expect(recommendationsSummary({ resolved: 0, new: 1, persists: 0 })).toBe('1 new');
    expect(recommendationsSummary({ resolved: 4, new: 0, persists: 1 })).toBe('4 resolved, 1 persist');
    expect(recommendationsSummary({ resolved: 0, new: 0, persists: 0 })).toBe('No recommendation in either run');
  });
});

describe('recommendationChanges', () => {
  it('gives nothing when the comparison has no section', () => {
    expect(recommendationChanges(different)).toBeUndefined();
  });

  const view = recommendationChanges({ recommendations: recommendationDiffs })!;

  it('words one row per item in the platform order, with its status and tone', () => {
    expect(view.summary).toBe('1 resolved, 1 new, 1 persist');
    expect(view.rows.map((r) => [r.title, r.statusLabel, r.tone, r.runs])).toEqual([
      ['Statement not tested', 'Resolved', 'pass', ['before']],
      ['Statement changed nothing', 'Persists', 'neutral', ['before', 'after']],
      ['Sampling policy shadowed', 'New', 'warn', ['after']],
    ]);
    expect(view.rows[0]).toMatchObject({ pipeline: 'logs', components: ['processor/transform'], subject: 'set(log.attributes["a"], "b") where false' });
    expect(new Set(view.rows.map((r) => r.key)).size).toBe(3);
  });

  it('shows the summary of the run that holds the recommendation, the after run for a persisting one', () => {
    expect(view.rows.map((r) => r.summary)).toEqual([
      'Statement 0 never matched in this run.',
      'Statement 1 changed nothing in 3 evaluations.',
      'Policy slow is shadowed by an earlier policy.',
    ]);
  });

  it('says how the benefit of a persisting recommendation moved, and nothing when it did not', () => {
    expect(view.rows.map((r) => r.benefitChange)).toEqual([undefined, '12 → 3 evaluations had no effect', undefined]);
    const persists = (before?: number, after?: number, metric = 'evaluations_without_effect'): RecommendationDiff => ({
      rule: 'x.y', status: 'persists', components: [],
      before: { index: 0, kind: 'redundancy', confidence: 'observed', summary: 's', benefit: before === undefined ? undefined : { metric, count: before } },
      after: { index: 0, kind: 'redundancy', confidence: 'observed', summary: 's', benefit: after === undefined ? undefined : { metric: 'evaluations_without_effect', count: after } },
    });
    const change = (d: RecommendationDiff) => recommendationChanges({ recommendations: { resolved: 0, new: 0, persists: 1, items: [d] } })!.rows[0].benefitChange;
    expect(change(persists(5, 5))).toBeUndefined();
    expect(change(persists(5))).toBeUndefined();
    expect(change(persists(undefined, 5))).toBeUndefined();
    expect(change(persists(5, 2, 'records_without_effect'))).toBeUndefined();
  });

  it('shows an unknown rule by its ID and an unknown status as it came, never hiding the item', () => {
    const r = recommendationChanges({ recommendations: { resolved: 0, new: 0, persists: 0, items: [
      { rule: 'future.rule', status: 'moved', components: [], after: { index: 2, kind: 'risk', confidence: 'static', summary: 'Something.' } },
    ] } })!.rows[0];
    expect(r).toMatchObject({ title: 'future.rule', statusLabel: 'moved', tone: 'neutral', runs: ['after'], summary: 'Something.', components: [] });
  });

  it('keeps an empty item list as a summary only', () => {
    expect(recommendationChanges({ recommendations: { resolved: 0, new: 0, persists: 0 } })).toEqual({ summary: 'No recommendation in either run', rows: [] });
  });
});
