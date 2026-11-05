// The path of a followed record journey as a row of steps for the component
// pane. It lists each graph node the record was observed at, in the order of
// its hops (a fan-out continues with the first branch before the next), and
// marks where it was stopped or delivered.
import type { Event, RecordLineage } from '../api/types';
import { hopNodeKey, hopTree, pathHighlight, type HopNode, type PathStatus } from '../inspect/journey';

export interface PathStep {
  /** Graph node key; selecting it opens that component. */
  key: string;
  label: string;
  status: PathStatus;
}

export function journeyPath(l: RecordLineage, events: Event[]): PathStep[] {
  const bySeq = new Map(events.map((e) => [e.seq, e]));
  const status = pathHighlight(l, bySeq).nodes;
  const steps: PathStep[] = [];
  const seen = new Set<string>();
  const visit = (n: HopNode) => {
    const key = hopNodeKey(n.hop, bySeq);
    if (key && !seen.has(key)) {
      seen.add(key);
      steps.push({ key, label: n.hop.component ?? key, status: status.get(key) ?? 'passed' });
    }
    n.children.forEach(visit);
  };
  hopTree(l).forEach(visit);
  return steps;
}
