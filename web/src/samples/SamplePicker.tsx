import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { Sample } from '../api/types';
import { getWorkspace, updateWorkspace, type WorkspaceState } from '../workspace/store';
import { isDirty, loaded, type Draft } from '../workspace/drafts';

/** The drafts a sample replaces: its configuration and its scenario. */
export const SAMPLE_DRAFTS: Draft[] = ['config', 'scenario'];

/**
 * samplePatch is the workspace patch that loads a sample: its configuration
 * and its scenario, whose inputs the samples API inlines, both as clean
 * baselines.
 */
export function samplePatch(ws: WorkspaceState, s: Sample): Partial<WorkspaceState> {
  return loaded(ws, {
    configYAML: s.config_yaml,
    configPath: `samples/${s.id}/config.yaml`,
    scenarioYAML: s.scenario_yaml,
    scenarioPath: `samples/${s.id}/scenario.yaml`,
    saved: {},
  }, SAMPLE_DRAFTS);
}

/**
 * loadSample replaces the editors' content with a sample: its configuration
 * and its scenario, whose inputs the samples API inlines.
 */
export function loadSample(s: Sample): boolean {
  const ws = getWorkspace();
  const dirty = SAMPLE_DRAFTS.some((d) => isDirty(ws, d));
  const same = ws.configYAML === s.config_yaml && ws.scenarioYAML === s.scenario_yaml;
  if (dirty && !same && !window.confirm('Replace the configuration, inputs and mocks in the editors with this sample? Download them first to keep them.')) {
    return false;
  }
  updateWorkspace(samplePatch(ws, s));
  return true;
}

export function useSamples() {
  return useQuery({ queryKey: ['samples'], queryFn: api.samples, staleTime: Infinity });
}

/** SampleList lists the samples; choosing one loads it. */
export function SampleList({ onLoad }: { onLoad(s: Sample): void }) {
  const q = useSamples();
  if (q.error) return <p className="error">Samples: {q.error.message}</p>;
  if (!q.data) return <p className="muted">Loading samples…</p>;
  return (
    <ul className="samples">
      {q.data.map((s) => (
        <li key={s.id}>
          <button type="button" className="sample" onClick={() => onLoad(s)}>
            <strong>{s.title}</strong>
            {s.expect === 'fail' && <span className="badge">has a bug</span>}
            <span className="muted">{s.description}</span>
          </button>
        </li>
      ))}
    </ul>
  );
}

/** SamplePicker is the top bar's samples menu. */
export function SamplePicker({ onLoaded }: { onLoaded(): void }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="sample-picker">
      <button type="button" aria-expanded={open} onClick={() => setOpen(!open)}>
        Samples ▾
      </button>
      {open && (
        <>
          <div className="backdrop" onClick={() => setOpen(false)} />
          <div className="popover" role="menu">
            <SampleList
              onLoad={(s) => {
                if (loadSample(s)) onLoaded();
                setOpen(false);
              }}
            />
          </div>
        </>
      )}
    </div>
  );
}
