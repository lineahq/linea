// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RunInputs } from '../api/types';
import { RunInputsPanel } from './RunInputs';

import { getWorkspace, updateWorkspace, type WorkspaceState } from '../workspace/store';
import { isDirty, loaded } from '../workspace/drafts';
import { parse } from 'yaml';

const blank: WorkspaceState = {
  configYAML: '', configPath: '', scenarioYAML: '', scenarioPath: '', compare: false,
  baselines: undefined, saved: undefined,
};

const runs: Record<string, RunInputs> = {
  quick: {
    run_id: 'quick', mode: 'quick', config: { text: 'run config\n', label: 'saved.yaml', sha256: 'aa', artifact: { artifact_id: 'c1', revision_id: 'rev1' } },
    fixture: { text: '{"run":true}', label: 'input.json', sha256: 'bb' }, receiver: 'otlp', files: [], required_environment: ['TOKEN'],
  },
  scenario: {
    run_id: 'scenario', mode: 'scenario', config: { text: 'run config\n', sha256: 'aa' },
    scenario: { text: 'version: 1\nname: run\n', label: 'tests/run.yaml', sha256: 'cc' }, files: ['input.json'], required_environment: [],
  },
};

let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  updateWorkspace(blank);
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const id = decodeURIComponent(url.split('/')[4]);
    if (url.endsWith('/export') && init?.method === 'POST') {
      return new Response(JSON.stringify({ run_id: id, file: `# Scenario exported from linea run ${id}.\nversion: 1\n`, required_environment: [] }), { status: 200 });
    }
    if (url.endsWith('/scenario') && init?.method === 'POST') {
      return new Response(JSON.stringify({ run_id: id, yaml: `version: 1\nname: from ${id}\n`, required_environment: runs[id].required_environment }), { status: 200 });
    }
    return new Response(JSON.stringify(runs[id]), { status: 200 });
  }));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); vi.unstubAllGlobals(); });

async function render(runId: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RunInputsPanel, { runId }))));
  for (let i = 0; i < 100 && !host.querySelector('.run-inputs'); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}
function button(text: string) {
  const b = [...host.querySelectorAll('button')].find((e) => e.textContent === text);
  if (!b) throw new Error(`no button ${text} in ${host.innerHTML}`);
  return b;
}
const click = (text: string) => act(() => button(text).click());

describe('opening a run as draft', () => {
  it('shows what the run was admitted with, names only for files and environment', async () => {
    await render('quick');
    expect(host.textContent).toContain('saved.yaml');
    expect(host.textContent).toContain('saved revision rev1');
    expect(host.textContent).toContain('TOKEN');
    await render('scenario');
    expect(host.textContent).toContain('tests/run.yaml');
    expect(host.textContent).toContain('input.json');
  });

  for (const mode of ['quick', 'scenario'] as const) {
    it(`asks before replacing dirty drafts of a ${mode} run`, async () => {
      updateWorkspace({ configYAML: 'my config\n', scenarioYAML: 'version: 1\nname: mine\n' });
      await render(mode);
      click("Open the run's configuration and scenario");
      const prompt = host.querySelector('[role="alertdialog"]')!;
      expect(prompt.textContent).toContain('Replace your unsaved configuration and scenario?');
      expect(prompt.getAttribute('aria-label')).toBe('Replace unsaved work');
      expect(prompt.textContent).toContain('replaces them');
      click('Keep mine');
      expect(getWorkspace().configYAML).toBe('my config\n');
      expect(host.querySelector('[role="alertdialog"]')).toBeNull();

      click("Open the run's configuration and scenario");
      click('Replace');
      const ws = getWorkspace();
      expect(ws.configYAML).toBe('run config\n');
      if (mode === 'quick') {
        const doc = parse(ws.scenarioYAML);
        expect(doc).toMatchObject({ version: 1, inputs: [{ receiver: 'otlp', data: '{"run":true}' }] });
        expect(ws.saved?.configuration).toEqual({ artifactId: 'c1', revisionId: 'rev1' });
      } else {
        expect(ws.scenarioYAML).toBe('version: 1\nname: run\n');
        expect(host.textContent).toContain('a rerun needs them there');
      }
    });
  }

  it('creates a scenario from the run in the document only, asking before replacing an unsaved one', async () => {
    updateWorkspace({ configYAML: 'my config\n', scenarioYAML: 'version: 1\nname: mine\n' });
    await render('quick');
    click('Create scenario from run');
    for (let i = 0; i < 100 && !host.querySelector('[role="alertdialog"]'); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
    expect(host.querySelector('[role="alertdialog"]')!.textContent).toContain('Replace your unsaved scenario?');
    expect(host.querySelector('[role="alertdialog"]')!.textContent).toContain('The scenario from run');
    expect(getWorkspace().scenarioYAML).toBe('version: 1\nname: mine\n');
    click('Replace');
    const ws = getWorkspace();
    expect(ws.scenarioYAML).toBe('version: 1\nname: from quick\n');
    expect(ws.configYAML).toBe('my config\n');
    expect(isDirty(ws, 'scenario')).toBe(true); // Not saved anywhere yet.
    expect(host.textContent).toContain(`"Open the run's configuration and scenario" loads it`);
    expect(host.textContent).toContain('A rerun needs environment value TOKEN');
  });

  it('replaces clean drafts without asking', async () => {
    updateWorkspace(loaded(getWorkspace(), { configYAML: 'loaded\n', scenarioYAML: 'version: 1\n' }, ['config', 'scenario']));
    await render('scenario');
    click("Open the run's configuration and scenario");
    expect(host.querySelector('[role="alertdialog"]')).toBeNull();
    expect(getWorkspace().configYAML).toBe('run config\n');
    expect(getWorkspace().scenarioYAML).toBe('version: 1\nname: run\n');
  });
});
