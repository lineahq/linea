// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SavedSources } from './SavedSources';
import { encodeText, openedPatch } from './drafts';
import { getWorkspace, updateWorkspace, type WorkspaceState } from './store';

const blank: WorkspaceState = {
  configYAML: '', configPath: '', scenarioYAML: '', scenarioPath: '', compare: false,
  baselines: undefined, saved: undefined,
};
const head = { id: 'a1', workspace_id: 'w', kind: 'configuration', label: 'otel.yaml', generation: 2, head_revision_id: 'rev2', created: '2026-10-13T00:00:00Z', updated: '2026-10-13T01:00:00Z' };

type Handler = (method: string, url: string, body?: unknown) => [number, unknown];
let handler: Handler;
let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  updateWorkspace(blank);
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const [status, body] = handler(init?.method ?? 'GET', url, init?.body ? JSON.parse(init.body as string) : undefined);
    return new Response(JSON.stringify(body), { status });
  }));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); vi.unstubAllGlobals(); });

async function settle(until: () => boolean) {
  for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}
async function render() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = () => createElement(QueryClientProvider, { client }, createElement(SavedSources, { ws: getWorkspace(), kind: 'configuration' }));
  await act(async () => root.render(view()));
  await settle(() => host.querySelector('button') !== null);
  return () => act(() => root.render(view()));
}
function click(text: string) {
  const b = [...host.querySelectorAll('button')].find((e) => e.textContent === text);
  if (!b) throw new Error(`no button ${text} in ${host.innerHTML}`);
  act(() => b.click());
}

describe('saved artifacts in the editors', () => {
  it('shows nothing when the server cannot list saved work', async () => {
    handler = () => [503, { code: 'unavailable', error: 'this server keeps no saved work' }];
    await render();
    await settle(() => false);
    expect(host.innerHTML).toBe('');
  });

  it('saves an edit against the revision it was opened from, and offers the head after a conflict', async () => {
    const saves: unknown[] = [];
    handler = (method, url, body) => {
      if (url.startsWith('/api/v1/artifacts?')) return [200, { workspace: { id: 'w' }, items: [head] }];
      if (url === '/api/v1/artifacts/a1') return [200, head];
      if (url === '/api/v1/artifacts/a1/revisions/rev2') return [200, { revision: {}, content_base64: encodeText('head text\n') }];
      if (method === 'POST') {
        saves.push(body);
        return saves.length === 1
          ? [201, { artifact: { ...head, id: 'a2', label: 'otel.yaml' }, revision: { ref: { revision_id: 'new1' } } }]
          : [409, { code: 'conflict', error: 'stale', current: head }];
      }
      return [404, { error: 'unexpected ' + url }];
    };
    // Opened from revision 1; the artifact's head has since moved to revision 2.
    updateWorkspace(openedPatch(getWorkspace(), 'configuration', 'old text\n', 'otel.yaml', { artifactId: 'a1', revisionId: 'rev1' }));
    updateWorkspace({ configYAML: 'my edit\n' });
    const rerender = await render();
    click('Save');
    await settle(() => host.querySelector('[aria-label="Saved artifact changed"]') !== null);
    expect(host.textContent).toContain('“otel.yaml” changed since you opened it');
    expect(saves).toEqual([]);

    click('Save as new');
    await settle(() => saves.length === 1 && getWorkspace().saved?.configuration?.artifactId === 'a2');
    expect(saves[0]).toMatchObject({ workspace_id: 'w', kind: 'configuration', label: 'otel.yaml', expected_generation: 0, content_base64: encodeText('my edit\n') });
    expect((saves[0] as Record<string, unknown>).artifact_id).toBeUndefined();
    expect(getWorkspace().saved?.configuration).toEqual({ artifactId: 'a2', revisionId: 'new1' });

    // A save that races another writer gets the server's 409 with the head.
    updateWorkspace(openedPatch(getWorkspace(), 'configuration', 'head text\n', 'otel.yaml', { artifactId: 'a1', revisionId: 'rev2' }));
    updateWorkspace({ configYAML: 'second edit\n' });
    rerender();
    click('Save');
    await settle(() => saves.length === 2 && host.querySelector('[aria-label="Saved artifact changed"]') !== null);
    expect(saves[1]).toMatchObject({ artifact_id: 'a1', expected_generation: 2, expected_head_revision_id: 'rev2' });
    click('Load it');
    await settle(() => getWorkspace().configYAML === 'head text\n');
    expect(getWorkspace().saved?.configuration).toEqual({ artifactId: 'a1', revisionId: 'rev2' });
  });

  it('asks before opening a saved revision over a dirty draft', async () => {
    handler = (_method, url) => {
      if (url.startsWith('/api/v1/artifacts?')) return [200, { workspace: { id: 'w' }, items: [head] }];
      if (url === '/api/v1/artifacts/a1/revisions/rev2') return [200, { revision: {}, content_base64: encodeText('head text\n') }];
      return [404, { error: 'unexpected ' + url }];
    };
    updateWorkspace({ configYAML: 'unsaved\n' });
    await render();
    click('Saved…');
    click('otel.yaml');
    await settle(() => host.querySelector('[aria-label="Replace unsaved work"]') !== null);
    expect(getWorkspace().configYAML).toBe('unsaved\n');
    click('Replace');
    expect(getWorkspace().configYAML).toBe('head text\n');
  });
});
