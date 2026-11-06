import { describe, expect, it } from 'vitest';
import type { GroupRequest } from '../api/types';
import { blockLabel, groupingFor, mergedBlocks, requestSummary, targets } from './grouping';
import { sampleGrouping } from './grouping.fixture';

const request = sampleGrouping.requests![0];

describe('grouping', () => {
  it('finds a processor by component and pipeline', () => {
    expect(groupingFor([sampleGrouping], 'groupbyattrs', 'logs')).toBe(sampleGrouping);
    expect(groupingFor([sampleGrouping], 'groupbyattrs', 'traces')).toBeUndefined();
    expect(groupingFor(undefined, 'groupbyattrs', 'logs')).toBeUndefined();
  });

  it('names blocks by service, then the other attributes', () => {
    expect(request.after!.map((b) => blockLabel(b.attributes))).toEqual([
      'checkout · host.name=node-1', 'checkout · host.name=node-2', 'checkout', 'cart · host.name=node-1',
    ]);
    expect(blockLabel([])).toBe('no attributes');
  });

  it('says where the records of each input block went', () => {
    expect([0, 1, 2].map((i) => targets(request, i))).toEqual([[1, 2, 3], [4], [3]]);
    expect(mergedBlocks(request)).toBe(1);
  });

  it('summarizes the request', () => {
    expect(requestSummary(request)).toBe(
      '3 resource blocks in, 4 resource blocks out. 4 records moved host.name to the resource; 2 without it kept their resource. 1 output block merged records from several input blocks with equal attributes.',
    );
  });

  it('summarizes a request without a key, without an emission, and with unfollowed records', () => {
    const kept: GroupRequest = { input: 2, output: 3, before: [{ records: 1 }], after: [{ records: [], from: [{ block: 0, records: 1 }] }], moved: [], grouped: 0, kept: 1, unfollowed: 2 };
    expect(requestSummary(kept)).toBe('1 resource block in, 1 resource block out. No record carried a grouping key, so every record kept its resource. 2 records could not be followed.');
    expect(requestSummary({ input: 2, before: [{ records: 1 }], grouped: 0, kept: 0, unfollowed: 0 })).toBe('1 resource block in; the processor emitted nothing for this request.');
  });
});
