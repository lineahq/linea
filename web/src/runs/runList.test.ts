// @vitest-environment jsdom
// TanStack Query never polls where there is no window.
import { InfiniteQueryObserver, QueryClient, QueryObserver } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RUN_LIST_POLL_MAX_PAGES, RUN_LIST_POLL_MS, runListPollInterval, runListQuery, runPagesQuery } from './runList';

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('run list', () => {
  it('shows a run created elsewhere after the poll interval', async () => {
    vi.useFakeTimers();
    let runs: unknown[] = [];
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ runs }), { status: 200 })));
    const client = new QueryClient();
    const observer = new QueryObserver(client, runListQuery);
    const seen: number[] = [];
    const unsubscribe = observer.subscribe((r) => r.data && seen.push(r.data.runs?.length ?? 0));
    await vi.advanceTimersByTimeAsync(10);
    expect(seen.at(-1)).toBe(0);

    // An agent submits a run through MCP on the same server.
    runs = [{ id: 'mcp-run', state: 'done', created: '2026-10-13T00:00:00Z' }];
    await vi.advanceTimersByTimeAsync(RUN_LIST_POLL_MS - 100);
    expect(seen.at(-1)).toBe(0);
    await vi.advanceTimersByTimeAsync(200);
    expect(seen.at(-1)).toBe(1);
    expect(runListQuery.refetchIntervalInBackground).toBe(false);
    unsubscribe();
    client.clear();
  });

  it('reads the newest page without a cursor', async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify({ runs: [], next_cursor: 'c1' }), { status: 200 }));
    vi.stubGlobal('fetch', fetch);
    const client = new QueryClient();
    const page = await client.fetchQuery(runListQuery);
    expect(page.next_cursor).toBe('c1');
    expect(fetch.mock.calls.map((c) => (c as unknown as [string])[0])).toEqual(['/api/v1/runs']);
    client.clear();
  });
});

describe('paged run list', () => {
  const runs = (...ids: string[]) => ids.map((id) => ({ id, state: 'done', created: '2026-10-30T00:00:00Z' }));
  const reply = (body: unknown) => new Response(JSON.stringify(body), { status: 200 });

  it('keys under runs and tells filters apart', () => {
    expect(runPagesQuery({}).queryKey.slice(0, 1)).toEqual(['runs']);
    expect(runPagesQuery({ result: 'failed' }).queryKey).not.toEqual(runPagesQuery({}).queryKey);
    // Unset and empty filters are the same list.
    expect(runPagesQuery({ scenario: '' }).queryKey).toEqual(runPagesQuery({}).queryKey);
    // The page's cursor is not part of the key.
    expect(runPagesQuery({ scenario: 'a', cursor: 'x' }).queryKey).toEqual(runPagesQuery({ scenario: 'a' }).queryKey);
  });

  it('follows next_cursor with the same filters until the last page', async () => {
    const urls: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      urls.push(url);
      if (url.includes('cursor=c2')) return reply({ runs: runs('a') });
      if (url.includes('cursor=c1')) return reply({ runs: runs('c', 'b'), next_cursor: 'c2' });
      return reply({ runs: runs('e', 'd'), next_cursor: 'c1' });
    }));
    const client = new QueryClient();
    const observer = new InfiniteQueryObserver(client, runPagesQuery({ scenario: 'checkout', result: 'failed' }));
    const unsubscribe = observer.subscribe(() => {});
    await vi.waitFor(() => expect(observer.getCurrentResult().data?.pages.length).toBe(1));
    expect(observer.getCurrentResult().hasNextPage).toBe(true);
    await observer.fetchNextPage();
    await observer.fetchNextPage();
    const result = observer.getCurrentResult();
    expect(result.data?.pages.flatMap((p) => p.runs?.map((r) => r.id))).toEqual(['e', 'd', 'c', 'b', 'a']);
    expect(result.hasNextPage).toBe(false);
    expect(urls).toEqual([
      '/api/v1/runs?scenario=checkout&result=failed',
      '/api/v1/runs?scenario=checkout&result=failed&cursor=c1',
      '/api/v1/runs?scenario=checkout&result=failed&cursor=c2',
    ]);
    unsubscribe();
    client.clear();
  });

  it('refreshes every loaded page on the poll interval, from the first', async () => {
    vi.useFakeTimers();
    const urls: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      urls.push(url);
      return url.includes('cursor=') ? reply({ runs: runs('a') }) : reply({ runs: runs('b'), next_cursor: 'c1' });
    }));
    const client = new QueryClient();
    const observer = new InfiniteQueryObserver(client, runPagesQuery({}));
    const unsubscribe = observer.subscribe(() => {});
    await vi.advanceTimersByTimeAsync(10);
    await observer.fetchNextPage();
    urls.length = 0;
    await vi.advanceTimersByTimeAsync(RUN_LIST_POLL_MS + 10);
    expect(urls).toEqual(['/api/v1/runs', '/api/v1/runs?cursor=c1']);
    unsubscribe();
    client.clear();
  });
});

describe('run list polling', () => {
  it('stops once more pages are loaded than a reader is likely to be watching', () => {
    expect(runListPollInterval(0)).toBe(RUN_LIST_POLL_MS);
    expect(runListPollInterval(RUN_LIST_POLL_MAX_PAGES)).toBe(RUN_LIST_POLL_MS);
    expect(runListPollInterval(RUN_LIST_POLL_MAX_PAGES + 1)).toBe(false);
  });
});
