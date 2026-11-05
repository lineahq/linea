import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { RunPruneResult, StorageUsage } from '../api/types';
import { Icon } from '../ui/Icon';
import { Crumbs } from '../ui/PageHead';
import { useDenial } from '../ui/session';
import { formatSize } from '../runtimes/runtimes';
import {
  canPrune, formProblem, NO_PRUNE_FORM, oldestText, previewMatches, previewText, PRUNE_RESULTS, pruneProblem, pruneQuestion, pruneRequest,
  prunedText, PROTECTED_TEXT, retentionText, storageProblem, usageView, type Preview, type PruneForm,
} from './runStorage';

/**
 * RunStoragePage (`/runs/storage`) shows what the run history holds against
 * its limit and the retention policy the server applies, and prunes runs by
 * hand. A prune is a dry run first: "Preview" counts the runs a
 * prune of the form's values would remove and the storage it would free, and
 * "Prune" is enabled only for those very values, then asks inline. Only
 * an admin may prune; the interface knows only that run.delete is needed, so
 * the server's refusal is worded here. The runtimes' Storage page is about
 * Docker's disk, not this.
 */
export function RunStoragePage() {
  const qc = useQueryClient();
  const denied = useDenial('run.delete');
  const storage = useQuery({ queryKey: ['run-storage'], queryFn: api.storageUsage, retry: false });
  const [form, setForm] = useState<PruneForm>(NO_PRUNE_FORM);
  const [preview, setPreview] = useState<Preview>();
  const [confirming, setConfirming] = useState(false);
  const [pruned, setPruned] = useState<RunPruneResult>();
  const dry = useMutation({
    mutationFn: (f: PruneForm) => api.pruneRuns(pruneRequest(f, true)),
    onSuccess: (result, f) => setPreview({ form: f, result }),
  });
  const prune = useMutation({
    mutationFn: (f: PruneForm) => api.pruneRuns(pruneRequest(f, false)),
    onSuccess: (result) => {
      setConfirming(false);
      setPreview(undefined);
      setPruned(result);
      void qc.invalidateQueries({ queryKey: ['run-storage'] });
      void qc.invalidateQueries({ queryKey: ['runs'] });
    },
  });
  const change = (patch: Partial<PruneForm>) => {
    setForm({ ...form, ...patch });
    setConfirming(false);
    dry.reset();
    prune.reset();
  };
  const problem = formProblem(form);
  const shown = previewMatches(form, preview) ? preview : undefined;
  const failure = dry.error ?? prune.error;

  return (
    <>
      <header className="od-wbhead">
        <Crumbs items={[{ label: 'Debugger', to: '/' }, { label: 'All runs', to: '/runs' }, { label: 'Storage' }]} />
        <span style={{ flex: 1 }} />
        <Link to="/runs" className="od-btn od-btn--ghost">
          <Icon name="arrow-left" size={16} />
          <span>Back to all runs</span>
        </Link>
      </header>
      <div className="od-page">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16, maxWidth: 980 }}>
          {storage.isPending && <p className="od-muted" role="status">Reading the run history's storage…</p>}
          {storage.error && (
            <div className="od-callout od-callout--warn" role="alert">
              <Icon name="triangle-alert" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">Storage cannot be shown</span>
                <div className="od-callout__body">{storageProblem(storage.error)}</div>
              </div>
              <div className="od-callout__actions">
                <button type="button" className="od-btn od-btn--sm" onClick={() => void storage.refetch()}>Try again</button>
              </div>
            </div>
          )}
          {storage.data && <Usage st={storage.data} />}
          <section className="od-card" aria-label="Prune runs">
            <div className="od-card__head">
              <h2 className="od-card__title">Prune runs</h2>
              <span className="od-subtle" style={{ fontSize: 12 }}>only an admin may prune · preview first</span>
            </div>
            <div className="od-card__body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
              <p className="od-muted" style={{ margin: 0, fontSize: 12.5 }}>
                Deletes finished runs by hand, as deleting each would. Each filter narrows the selection; with none, every finished run that retention could remove is selected. {PROTECTED_TEXT}
              </p>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 12, alignItems: 'flex-end' }}>
                <div className="od-field">
                  <label htmlFor="prune-before">Admitted before</label>
                  <input id="prune-before" type="date" className="od-input" value={form.before} aria-invalid={!!problem} onChange={(e) => change({ before: e.target.value })} />
                </div>
                <div className="od-field">
                  <label htmlFor="prune-result">Result</label>
                  <select id="prune-result" className="od-select" value={form.result} onChange={(e) => change({ result: e.target.value })}>
                    <option value="">Any finished result</option>
                    {PRUNE_RESULTS.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
                  </select>
                </div>
                <div className="od-field">
                  <label htmlFor="prune-scenario">Scenario</label>
                  <input id="prune-scenario" className="od-input" placeholder="any" value={form.scenario} onChange={(e) => change({ scenario: e.target.value })} />
                </div>
                <div className="od-field">
                  <label htmlFor="prune-runtime">Runtime</label>
                  <input id="prune-runtime" className="od-input" placeholder="any" value={form.runtime} onChange={(e) => change({ runtime: e.target.value })} />
                </div>
                <div className="od-field">
                  <label htmlFor="prune-project">Project (optional)</label>
                  <input id="prune-project" className="od-input" placeholder="every project you administer" value={form.project} onChange={(e) => change({ project: e.target.value })} />
                </div>
              </div>
              {problem && <div className="od-field__error" role="alert">{problem}</div>}
              {confirming && shown ? (
                <div className="od-confirm" role="alertdialog" aria-label="Confirm prune">
                  <Icon name="triangle-alert" size={16} />
                  <span className="od-confirm__text">{pruneQuestion(form, shown.result)}</span>
                  <button type="button" className="od-btn od-btn--sm" onClick={() => setConfirming(false)}>Cancel</button>
                  <button type="button" className="od-btn od-btn--danger od-btn--sm" disabled={prune.isPending} onClick={() => prune.mutate(form)}>
                    {prune.isPending ? 'Deleting…' : `Delete ${shown.result.runs.toLocaleString('en-US')} ${shown.result.runs === 1 ? 'run' : 'runs'}`}
                  </button>
                </div>
              ) : (
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                  <button
                    type="button" className="od-btn" disabled={!!problem || dry.isPending || !!denied} title={denied}
                    onClick={() => { prune.reset(); setPruned(undefined); dry.mutate(form); }}
                  >
                    <Icon name="eye" size={14} /><span>{dry.isPending ? 'Counting…' : 'Preview'}</span>
                  </button>
                  <button
                    type="button" className="od-btn od-btn--danger" disabled={!canPrune(form, preview) || !!denied}
                    title={denied ?? (canPrune(form, preview) ? 'Delete the runs the preview counted' : 'Preview these values first')}
                    onClick={() => setConfirming(true)}
                  >
                    <Icon name="trash-2" size={14} /><span>Prune</span>
                  </button>
                  {shown && <span role="status" className="t-body-strong">{previewText(shown.result)}</span>}
                </div>
              )}
              {failure && (
                <div className="od-callout od-callout--fail" role="alert">
                  <Icon name="octagon-alert" size={16} />
                  <div className="od-callout__main"><div className="od-callout__title">{pruneProblem(failure)}</div></div>
                </div>
              )}
              {pruned && (
                <div className="od-callout od-callout--pass" role="status">
                  <Icon name="circle-check" size={16} />
                  <div className="od-callout__main"><div className="od-callout__title">{prunedText(pruned)}</div></div>
                </div>
              )}
            </div>
          </section>
        </div>
      </div>
    </>
  );
}

function Usage({ st }: { st: StorageUsage }) {
  const use = usageView(st);
  return (
    <section className="od-card" aria-label="Run history storage">
      <div className="od-card__head">
        <h2 className="od-card__title">Run history</h2>
        <span style={{ flex: 1 }} />
        <span className="od-mono" style={{ fontSize: 12 }}>{use.text}</span>
      </div>
      <div className="od-card__body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {use.percent !== undefined && (
          <div className="od-meter" role="img" aria-label={use.summary}>
            <span style={{ width: `${use.percent}%`, background: use.over ? 'var(--fail)' : 'var(--accent)' }} />
          </div>
        )}
        {use.over && <div className="od-field__error" role="alert">More is stored than the limit allows, so new runs are refused until runs are removed.</div>}
        <dl className="od-kv" style={{ margin: 0 }}>
          <dt>Runs</dt><dd>{st.runs.toLocaleString('en-US')}</dd>
          <dt>Kept runs</dt><dd>{st.kept_runs.toLocaleString('en-US')}</dd>
          <dt>Oldest run</dt><dd>{oldestText(st)}</dd>
          {st.run_limit > 0 && (<><dt>One run may reserve</dt><dd>{formatSize(st.run_limit)}</dd></>)}
          <dt>Retention</dt><dd>{retentionText(st.retention)}</dd>
        </dl>
        <p className="od-muted" style={{ margin: 0, fontSize: 12.5 }}>{PROTECTED_TEXT}</p>
      </div>
    </section>
  );
}
