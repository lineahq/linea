import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/client';
import type { StorageUsage } from '../api/types';
import {
  ageText, beforeTime, canPrune, formProblem, isUnfiltered, NO_PRUNE_FORM, oldestText, previewMatches, previewText, PRUNE_RESULTS,
  pruneProblem, pruneQuestion, pruneRequest, prunedText, retentionText, sameForm, scopeText, storageProblem, usageView, type Preview, type PruneForm,
} from './runStorage';

const usage = (patch: Partial<StorageUsage> = {}): StorageUsage => ({
  limit: 4_000_000_000, run_limit: 100_000_000, used: 1_000_000_000, runs: 120, kept_runs: 3, retention: { runs: 10_000, age_seconds: 30 * 86_400 }, ...patch,
});
const form = (patch: Partial<PruneForm> = {}): PruneForm => ({ ...NO_PRUNE_FORM, ...patch });

describe('retention policy text', () => {
  it('words a count and an age', () => {
    expect(retentionText({ runs: 10_000, age_seconds: 30 * 86_400 })).toBe('At most 10,000 runs, removed after 30 days.');
  });
  it('words one bound alone and no bound', () => {
    expect(retentionText({ runs: 1, age_seconds: 0 })).toBe('At most 1 run.');
    expect(retentionText({ runs: 0, age_seconds: 86_400 })).toBe('Runs are removed after 1 day.');
    expect(retentionText({ runs: 0, age_seconds: 0 })).toBe('No bound on the number or the age of runs.');
  });
  it('words an age in the largest whole unit', () => {
    expect([ageText(7 * 86_400), ageText(36 * 3600), ageText(5400), ageText(120), ageText(45), ageText(3600)]).toEqual(
      ['7 days', '36 hours', '90 minutes', '2 minutes', '45 seconds', '1 hour'],
    );
  });
});

describe('usageView', () => {
  it('compares the stored bytes with the limit', () => {
    expect(usageView(usage())).toEqual({ text: '1.0 GB of 4.0 GB', percent: 25, over: false, summary: '1.0 GB of 4.0 GB, 25% in use' });
  });
  it('flags use above a lowered limit and caps the meter', () => {
    expect(usageView(usage({ used: 5_000_000_000 }))).toMatchObject({ percent: 100, over: true });
  });
  it('has no share without a limit', () => {
    expect(usageView(usage({ limit: 0, used: 0 }))).toEqual({ text: '0 B used, no limit', over: false, summary: '0 B used, no storage limit' });
  });
});

describe('oldestText and storageProblem', () => {
  it('names the oldest run or says there is none', () => {
    expect(oldestText(usage())).toBe('No runs stored');
    expect(oldestText(usage({ oldest_run: '2026-10-01T10:00:00Z' }), new Date('2026-10-30T12:00:00Z'))).toMatch(/^Oct 1 \d\d:\d\d$/);
  });
  it('words a refused or unavailable read', () => {
    expect(storageProblem(new ApiError(403, 'forbidden'))).toMatch(/may not read saved work/);
    expect(storageProblem(new ApiError(503, 'x'))).toMatch(/keeps no saved work/);
    expect(storageProblem(new ApiError(500, 'boom'))).toBe('Could not read the storage: boom');
  });
});

describe('prune form', () => {
  it('offers only the results of finished runs, as the run list words them', () => {
    expect(PRUNE_RESULTS.map(([v]) => v)).toEqual(['completed', 'partial', 'failed', 'cancelled', 'error', 'lost']);
    expect(PRUNE_RESULTS.find(([v]) => v === 'partial')![1]).toBe('ended early');
  });

  it('turns a date into the start of that day and refuses anything else', () => {
    expect(beforeTime('2026-10-01')).toBe(new Date(2026, 9, 1).toISOString());
    expect(beforeTime('')).toBeUndefined();
    expect(beforeTime('2026-02-31')).toBeUndefined();
    expect(beforeTime('1 Oct')).toBeUndefined();
    expect(formProblem(form({ before: '1 Oct' }))).toBe('Enter the date as year-month-day.');
    expect(formProblem(form({ before: '2026-10-01' }))).toBeUndefined();
    expect(formProblem(form())).toBeUndefined();
  });

  it('sends only the filters that are set, and the dry run flag only for a preview', () => {
    expect(pruneRequest(form(), false)).toEqual({});
    expect(pruneRequest(form(), true)).toEqual({ dry_run: true });
    expect(pruneRequest(form({ before: '2026-10-01', result: 'failed', scenario: ' checkout ', runtime: 'built-in', project: 'team-a' }), true)).toEqual({
      before: new Date(2026, 9, 1).toISOString(), result: 'failed', scenario: 'checkout', runtime: 'built-in', project: 'team-a', dry_run: true,
    });
  });

  it('sees whether the form selects every prunable run', () => {
    expect(isUnfiltered(form())).toBe(true);
    expect(isUnfiltered(form({ scenario: '  ' }))).toBe(true);
    expect(isUnfiltered(form({ project: 'team-a' }))).toBe(true);
    expect(isUnfiltered(form({ result: 'failed' }))).toBe(false);
  });
});

describe('preview of the same form', () => {
  const preview = (f: PruneForm, runs = 4): Preview => ({ form: f, result: { runs, bytes: 2_500, dry_run: true } });

  it('matches only the form values it was made for, ignoring surrounding blanks', () => {
    const f = form({ result: 'failed', scenario: 'checkout' });
    expect(sameForm(f, { ...f, scenario: ' checkout ' })).toBe(true);
    expect(previewMatches(f, preview(f))).toBe(true);
    for (const changed of [{ before: '2026-10-01' }, { result: 'error' }, { scenario: 'other' }, { runtime: 'x' }, { project: 'p' }]) {
      expect(previewMatches({ ...f, ...changed }, preview(f))).toBe(false);
    }
    expect(previewMatches(f, undefined)).toBe(false);
  });

  it('enables the prune only after a preview of this form that found runs', () => {
    const f = form({ result: 'failed' });
    expect(canPrune(f, undefined)).toBe(false);
    expect(canPrune(f, preview(f))).toBe(true);
    expect(canPrune({ ...f, result: 'lost' }, preview(f))).toBe(false);
    expect(canPrune(f, preview(f, 0))).toBe(false);
    expect(canPrune(form({ before: 'soon' }), preview(form({ before: 'soon' })))).toBe(false);
  });
});

describe('prune wording', () => {
  it('words a preview and a finished prune', () => {
    expect(previewText({ runs: 1, bytes: 1_500, dry_run: true })).toBe('1 run, 1.5 kB freed');
    expect(previewText({ runs: 1200, bytes: 3_200_000_000, dry_run: true })).toBe('1,200 runs, 3.2 GB freed');
    expect(previewText({ runs: 0, bytes: 0, dry_run: true })).toBe('No run matches, so there is nothing to remove.');
    expect(prunedText({ runs: 2, bytes: 2_000_000, dry_run: false })).toBe('Removed 2 runs, freed 2.0 MB.');
    expect(prunedText({ runs: 0, bytes: 0, dry_run: false })).toBe('No run was removed.');
  });

  it('names what the form selects', () => {
    expect(scopeText(form())).toBe('every finished run');
    expect(scopeText(form({ before: '2026-10-01', result: 'partial', scenario: 'checkout', runtime: 'built-in', project: 'team-a' }))).toBe(
      'finished runs admitted before 2026-10-01, with result ended early, of scenario checkout, on runtime built-in, in project team-a',
    );
  });

  it('asks with the count, the filters, the irreversibility and what stays', () => {
    expect(pruneQuestion(form({ result: 'failed' }), { runs: 3, bytes: 4_000, dry_run: true })).toBe(
      'Delete 3 runs (finished runs with result failed), freeing 4.0 kB? Their reports, recorded evidence and captured inputs are removed for good. Kept runs, runs of a suite and runs a saved source references stay.',
    );
  });

  it('says only an admin may prune on a refusal', () => {
    expect(pruneProblem(new ApiError(403, 'forbidden: admin required'))).toBe('Only an admin may prune runs.');
    expect(pruneProblem(new ApiError(503, 'x'))).toMatch(/keeps no run history/);
    expect(pruneProblem(new ApiError(400, 'unknown result "x"'))).toBe('unknown result "x"');
  });
});
