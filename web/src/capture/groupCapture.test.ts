import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import { loadFleetGroupMembers, resolveGroupCapture } from './groupCapture';
import type { Collector } from '../api/types';
const member = (id: string): Collector => ({ id, generation: 1, credential_id: 'cred', pod: id, cluster: 'lab', namespace: 'telemetry', collector_group: 'gateways', enabled: true, point_count: 1, raw_point_count: 0, collector_version: '0.162.0', distribution_version: 'dev', filter_limits: { max_conditions: 16, max_condition_chars: 256, max_regex_chars: 256 } });
afterEach(() => vi.restoreAllMocks());
it('refuses oversized groups without truncating or fetching point catalogs', async () => {
 vi.spyOn(api, 'collectors').mockResolvedValue({ enabled: true, items: Array.from({ length: 17 }, (_, i) => member(String(i))) });
 const points = vi.spyOn(api, 'capturePoints');
 await expect(resolveGroupCapture('cluster', 'lab')).rejects.toThrow('more than 16'); expect(points).not.toHaveBeenCalled();
});
it('refuses changing inventory and repeated cursors instead of returning partial targets', async () => {
 const inventory = vi.spyOn(api, 'collectors').mockResolvedValueOnce({ enabled: true, items: [member('one')], next_cursor: 'next' }).mockRejectedValueOnce(new Error('Inventory changed'));
 await expect(resolveGroupCapture('cluster', 'lab')).rejects.toThrow('Inventory changed');
 inventory.mockResolvedValue({ enabled: true, items: [member('one')], next_cursor: 'repeat' });
 await expect(resolveGroupCapture('cluster', 'lab')).rejects.toThrow('paging repeated');
});
it('reads every point page and offers only common points, using the stronger sensitivity', async () => {
 vi.spyOn(api, 'collectors').mockResolvedValue({ enabled: true, items: [member('one'), member('two')] });
 vi.spyOn(api, 'capturePoints').mockImplementation(async (id, cursor) => ({ collector_id: id, items: cursor ? [{ pipeline: 'logs', point: 'egress', signal: 'logs', sensitivity: id === 'two' ? 'raw' : 'redacted' }] : [{ pipeline: id === 'one' ? 'logs' : 'traces', point: 'ingress', signal: id === 'one' ? 'logs' : 'traces', sensitivity: 'redacted' }], next_cursor: cursor ? '' : 'more' }));
 const group = await resolveGroupCapture('cluster', 'lab');
 expect(group.points).toEqual([{ pipeline: 'logs', point: 'egress', signal: 'logs', sensitivity: 'raw' }]); expect(group.collectors).toHaveLength(2);
});

it('previews a fleet group from its connected members and counts offline agents', async () => {
 vi.spyOn(api, 'collectorGroup').mockResolvedValue({ id: 'edge', revision: 3, name: 'Edge', kind: 'static' });
 vi.spyOn(api, 'collectorGroupMembers').mockImplementation(async (_id, cursor) => cursor
  ? { group_id: 'edge', capture_linked: true, items: [{ agent_id: 'a3', connected: false }] }
  : { group_id: 'edge', capture_linked: true, items: [{ agent_id: 'a1', connected: true, collector_ids: ['one'] }, { agent_id: 'a2', connected: true, collector_ids: ['gone'] }], next_cursor: 'more' });
 const group = await loadFleetGroupMembers('edge', [member('one'), member('two')]);
 expect(group).toMatchObject({ name: 'Edge', offline: 1 });
 expect(group.collectors.map(c => c.id)).toEqual(['one']);
});

it('refuses repeated member pages instead of returning a partial group', async () => {
 vi.spyOn(api, 'collectorGroup').mockResolvedValue({ id: 'edge', revision: 1, name: 'Edge', kind: 'dynamic', selector: 'env=prod' });
 vi.spyOn(api, 'collectorGroupMembers').mockResolvedValue({ group_id: 'edge', capture_linked: true, items: [], next_cursor: 'again' });
 await expect(loadFleetGroupMembers('edge', [])).rejects.toThrow('paging repeated');
});
