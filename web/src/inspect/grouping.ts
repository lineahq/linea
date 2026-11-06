// Reading the report's grouping: what a groupbyattrs processor did to each
// request. The server derives it from the run's record lineage
// (internal/debugger/analysis/grouping.go, docs/internals/grouping.md); this
// module only names and summarizes it.
import type { GroupAttribute, GroupRequest, Grouping } from '../api/types';

/** groupingFor finds the grouping of one groupbyattrs processor in one pipeline. */
export function groupingFor(grouping: Grouping[] | undefined, component: string, pipeline?: string): Grouping | undefined {
  return grouping?.find((g) => g.processor === component && g.pipeline === pipeline);
}

/** blockLabel names a resource block by its attributes, the way a reader tells them apart. */
export function blockLabel(attrs: GroupAttribute[] | undefined): string {
  const list = attrs ?? [];
  const name = list.find((a) => a.key === 'service.name');
  const rest = list.filter((a) => a !== name);
  const parts = [name?.value, ...rest.map((a) => `${a.key}=${a.value}`)].filter(Boolean);
  return parts.length ? parts.join(' · ') : 'no attributes';
}

/** targets are the output blocks (1-based) that records of input block i ended in. */
export function targets(r: GroupRequest, i: number): number[] {
  return (r.after ?? []).flatMap((b, n) => ((b.from ?? []).some((f) => f.block === i) ? [n + 1] : []));
}

/** mergedBlocks counts the output blocks whose records came from several input blocks. */
export function mergedBlocks(r: GroupRequest): number {
  return (r.after ?? []).filter((b) => (b.from ?? []).length > 1).length;
}

/** requestSummary is the one sentence above a request's view. */
export function requestSummary(r: GroupRequest): string {
  const blocks = (n: number) => `${n} resource block${n === 1 ? '' : 's'}`;
  const before = r.before?.length ?? 0;
  if (r.output === undefined) return `${blocks(before)} in; the processor emitted nothing for this request.`;
  const moved = r.moved ?? [];
  const parts = [`${blocks(before)} in, ${blocks(r.after?.length ?? 0)} out.`];
  if (moved.length) parts.push(`${r.grouped} record${r.grouped === 1 ? '' : 's'} moved ${moved.join(', ')} to the resource; ${r.kept} without ${moved.length === 1 ? 'it' : 'them'} kept their resource.`);
  else parts.push('No record carried a grouping key, so every record kept its resource.');
  const merged = mergedBlocks(r);
  if (merged) parts.push(`${merged} output block${merged === 1 ? '' : 's'} merged records from several input blocks with equal attributes.`);
  if (r.unfollowed) parts.push(`${r.unfollowed} record${r.unfollowed === 1 ? '' : 's'} could not be followed.`);
  return parts.join(' ');
}
