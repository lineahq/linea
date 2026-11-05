// "Follow in both": one changed record's path through each compared
// run, side by side. The platform lists each run's path and names where they
// diverge; this module only lines the two paths up for display.
import type { ComparisonRecord, ComparisonStep } from '../api/types';
import { END_LABELS } from '../inspect/journey';

export interface FollowRow {
  key: string;
  /** The component, with its pipeline when known, as the platform names it in diverges_at. */
  label: string;
  before?: string;
  after?: string;
  /** This is the step where the platform says the runs diverge. */
  diverges: boolean;
  /** The two runs did something different here, or only one run reached it. */
  differs: boolean;
}

const where = (s: Pick<ComparisonStep, 'component' | 'pipeline'>) => (s.pipeline ? `${s.component} (${s.pipeline})` : s.component);

/** stepText says what one component did with the record in one run. */
export function stepText(s: ComparisonStep): string {
  if (s.end) return END_LABELS[s.end] ?? s.end;
  if (s.changes) return `changed ${s.changes} field${s.changes === 1 ? '' : 's'}`;
  return 'passed';
}

/**
 * followRows lines up the record's two paths: the before path in its order,
 * with each step only the after path has placed after the step it follows.
 */
export function followRows(r: Pick<ComparisonRecord, 'before' | 'after' | 'diverges_at'>): FollowRow[] {
  const before = r.before.path ?? [], after = r.after.path ?? [];
  const inAfter = new Map(after.map((s) => [where(s), s]));
  const inBefore = new Set(before.map(where));
  const rows: FollowRow[] = [];
  const row = (label: string, b?: ComparisonStep, a?: ComparisonStep) => {
    const bt = b && stepText(b), at = a && stepText(a);
    rows.push({ key: label, label, before: bt, after: at, diverges: label === r.diverges_at, differs: !b || !a || bt !== at || b.changes !== a.changes });
  };
  // After-only steps before the first shared step come first.
  let pending: ComparisonStep[] = [];
  const flush = () => { for (const s of pending) row(where(s), undefined, s); pending = []; };
  const afterOnlyFollowing = new Map<string, ComparisonStep[]>();
  let previous = '';
  for (const s of after) {
    const k = where(s);
    if (inBefore.has(k)) { previous = k; continue; }
    if (!previous) { pending.push(s); continue; }
    afterOnlyFollowing.set(previous, [...(afterOnlyFollowing.get(previous) ?? []), s]);
  }
  flush();
  for (const s of before) {
    const k = where(s);
    row(k, s, inAfter.get(k));
    for (const a of afterOnlyFollowing.get(k) ?? []) row(where(a), undefined, a);
  }
  return rows;
}
