import { useState } from 'react';
import { api } from '../api/client';
import { downloadBlob } from '../files';
import { CI_TEST_COMMAND, exportPlanText, openExportPlan } from '../saved/ciArchive';
import { Icon } from '../ui/Icon';
import { isDirty, kindDraft } from '../workspace/drafts';
import { getWorkspace, type WorkspaceState } from '../workspace/store';
import { useSavedDraft } from '../workspace/useSavedDraft';

/**
 * ExportForCI is the one download: Export for CI of what is open. It saves
 * the configuration and the scenario first when they are not saved or have
 * changed, then downloads the CI archive of their saved revisions
 * (docs/ci.md), and says what to do with it. The Save menu and a run's More
 * menu (after opening the run's configuration and scenario) both show it.
 */
export function ExportForCI({ ws }: { ws: WorkspaceState }) {
  const savers = { configuration: useSavedDraft('configuration'), scenario: useSavedDraft('scenario') };
  const loading = savers.configuration.list.isPending || savers.scenario.list.isPending;
  const plan = openExportPlan(ws, { configuration: savers.configuration.list.data?.items, scenario: savers.scenario.list.data?.items });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [done, setDone] = useState(false);
  const run = async () => {
    if ('blocker' in plan) return;
    setBusy(true);
    setError(undefined);
    setDone(false);
    try {
      for (const kind of plan.saves) {
        await savers[kind].save(false);
        const now = getWorkspace();
        if (!now.saved?.[kind] || isDirty(now, kindDraft(kind))) return;
      }
      const now = getWorkspace();
      downloadBlob('collector-ci.zip', await api.exportForCI(now.saved!.configuration!.artifactId, [now.saved!.scenario!.artifactId]));
      setDone(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const problems = (['configuration', 'scenario'] as const).flatMap((k) => {
    const sv = savers[k];
    if (sv.conflict) return [`The ${k} was not saved: “${sv.conflict.label}” changed since you opened it. Save it above, then export.`];
    return sv.message && !sv.message.startsWith('Saved as') ? [`The ${k} was not saved: ${sv.message}`] : [];
  });
  return (
    <div style={{ padding: '12px 14px', display: 'flex', flexDirection: 'column', gap: 8 }} role="group" aria-label="Export for CI">
      <div className="t-subheading">Export for CI</div>
      <div className="od-muted" style={{ fontSize: 12 }}>
        {loading ? 'Checking saved work…' : 'blocker' in plan ? plan.blocker : exportPlanText(plan.saves)} Unpack it at your repository root and set the pinned debugger build in its workflow. Each pull request then runs:
      </div>
      <code style={{ fontSize: 11.5, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{CI_TEST_COMMAND}</code>
      <div>
        <button type="button" className="od-btn od-btn--sm" disabled={loading || 'blocker' in plan || busy} onClick={() => void run()}>
          <Icon name="download" size={14} />
          <span>{busy ? 'Exporting…' : 'Export for CI'}</span>
        </button>
      </div>
      {problems.map((p) => <span key={p} className="od-field__error" role="alert">{p}</span>)}
      {error && <span className="od-field__error" role="alert">The CI archive was not exported: {error}</span>}
      {done && <span className="od-muted" style={{ fontSize: 12 }} role="status">Downloaded collector-ci.zip. Its scenario may hold recorded telemetry: review it before committing.</span>}
    </div>
  );
}
