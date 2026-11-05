import { useId, useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { api } from '../api/client';
import type { ArtifactMetadata } from '../api/types';
import { Icon } from '../ui/Icon';
import { BUILT_IN, runtimeOptions } from '../runtimes/runtimes';
import { defaultRuntimeQuery, runtimesQuery, supportQuery } from '../runtimes/RuntimeSelect';
import { downloadBlob } from '../files';
import { useWorkspace } from '../workspace/store';
import { ciExportBlocker, savedConfigId } from './ciArchive';
import { buildSuiteRequest, configOptions, defaultConfig, suiteBlocker } from './suiteRequest';
import { useDenial } from '../ui/session';

/**
 * SuiteLauncher runs saved scenarios as a suite: it asks which configuration
 * they run against (the current draft or a saved configuration) and which
 * runtime, then submits the suite and opens its result. The runtime here is
 * a parameter of this suite only; it starts at the runtime chosen beside Run
 * and never changes it. The same selection with a saved configuration
 * exports for CI: a zip of the saved revisions and a GitHub Actions workflow.
 */
export function SuiteLauncher({
  members,
  configs,
  heading,
  note = 'each runs at its latest saved revision',
  onClear,
}: {
  members: ArtifactMetadata[];
  /** The saved configurations, at their head revisions. */
  configs: ArtifactMetadata[];
  heading: ReactNode;
  note?: string;
  onClear?: () => void;
}) {
  const ws = useWorkspace();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const id = useId();

  const draft = useMemo(() => ({ yaml: ws.configYAML, path: ws.configPath }), [ws.configYAML, ws.configPath]);
  const options = useMemo(() => configOptions(draft, configs), [draft, configs]);
  const [picked, setPicked] = useState<string>();
  const config = options.some((o) => o.value === picked) ? picked! : defaultConfig(options);

  const runtimes = useQuery(runtimesQuery);
  const support = useQuery(supportQuery);
  const team = useQuery(defaultRuntimeQuery);
  const [runtimePicked, setRuntimePicked] = useState<string>();
  const runtime = runtimePicked ?? ws.runtime ?? BUILT_IN;
  const runtimeChoices = runtimeOptions(runtimes.data, runtime, support.data?.built_in, team.data?.name);

  const blocker = suiteBlocker(members.length, config, options);
  const runDenied = useDenial('run.submit');
  const submit = useMutation({
    mutationFn: () => {
      const req = buildSuiteRequest(config, draft, configs, members, runtime);
      if (!req) return Promise.reject(new Error('The chosen configuration is not available.'));
      return api.submitSuite(req);
    },
    onSuccess: (suite) => {
      void qc.invalidateQueries({ queryKey: ['suites'] });
      void navigate({ to: '/saved/suites/$suiteId', params: { suiteId: suite.id } });
    },
  });

  const exportBlocker = ciExportBlocker(config, members);
  const exported = useMutation({
    mutationFn: () => api.exportForCI(savedConfigId(config)!, members.map((m) => m.id)),
    onSuccess: (blob) => downloadBlob('collector-ci.zip', blob),
  });

  return (
    <div className="saved-stack">
      <div className="saved-bar" role="group" aria-label="Run as suite">
        <div className="saved-bar__what">
          <span className="t-body-strong">{heading}</span>
          <span className="od-muted" style={{ fontSize: 12.5 }}>{note}</span>
        </div>
        <div className="od-field">
          <label htmlFor={`${id}-config`}>Configuration</label>
          <select id={`${id}-config`} className="od-select" value={config} disabled={options.length === 0} onChange={(e) => setPicked(e.target.value)}>
            {options.length === 0 && <option value="">None available</option>}
            {options.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </div>
        <div className="od-field">
          <label htmlFor={`${id}-runtime`}>Runtime for this suite</label>
          <select id={`${id}-runtime`} className="od-select" value={runtime} onChange={(e) => setRuntimePicked(e.target.value)}>
            {runtimeChoices.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </div>
        <span className="saved-grow" />
        {onClear && (
          <button type="button" className="od-btn od-btn--ghost" onClick={onClear} disabled={submit.isPending}>
            Clear selection
          </button>
        )}
        <button type="button" className="od-btn od-btn--lg" disabled={exportBlocker !== undefined || exported.isPending} title={exportBlocker ?? 'Download a zip of the saved configuration and scenarios with a GitHub Actions workflow that runs them'} onClick={() => exported.mutate()}>
          <Icon name="download" size={14} />
          <span>{exported.isPending ? 'Exporting…' : 'Export for CI'}</span>
        </button>
        <button type="button" className="od-btn od-btn--primary od-btn--lg" disabled={blocker !== undefined || submit.isPending || !!runDenied} title={runDenied ?? blocker} onClick={() => submit.mutate()}>
          <Icon name="play" size={14} />
          <span>{submit.isPending ? 'Starting…' : 'Run as suite'}</span>
        </button>
      </div>
      {blocker && members.length > 0 && <div className="od-field__help" role="status">{blocker}</div>}
      {exported.isError && (
        <div className="od-callout od-callout--fail" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main">
            <div className="od-callout__title">The CI archive was not exported</div>
            <div className="od-callout__body">{exported.error instanceof Error ? exported.error.message : String(exported.error)}</div>
          </div>
        </div>
      )}
      {exported.isSuccess && <div className="od-field__help" role="status">Downloaded collector-ci.zip. Its scenarios may hold recorded telemetry: review the files before committing them.</div>}
      {submit.isError && (
        <div className="od-callout od-callout--fail" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main">
            <div className="od-callout__title">The suite did not start</div>
            <div className="od-callout__body">{submit.error instanceof Error ? submit.error.message : String(submit.error)}</div>
          </div>
        </div>
      )}
    </div>
  );
}
