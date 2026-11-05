import { useEffect, useRef, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, useParams, useRouter, useSearch } from '@tanstack/react-router';
import { api, ApiError } from '../../api/client';
import { Crumbs } from '../../ui/PageHead';
import { Icon } from '../../ui/Icon';
import { useDenial } from '../../ui/session';
import { getWorkspace, updateWorkspace } from '../../workspace/store';
import { browserStorage, recallBuild } from '../buildMemory';
import { finished, problemOf, UNVERIFIED } from '../runtimes';
import { useBuildLog } from '../useBuildLog';
import { buildElapsed, buildOutcome, logText, numberedLog } from './buildView';
import { fromOf, rebuildHref, returnTarget, setupTitle } from './returnPath';
import { buildChip } from './runtimeList';
import { StatePill } from './StatePill';
import './runtimes.css';

/** MANIFEST_LINES caps the manifest shown; the whole text is what the build used. */
const MANIFEST_LINES = 300;

/** useNow ticks every `ms` while it is given one, so an elapsed time keeps counting. */
function useNow(ms: number | undefined): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!ms) return;
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

/**
 * BuildPage follows one runtime build: its state, its live log (new lines
 * only, over a server-sent stream) and Cancel. A build keeps running if the
 * user leaves. When it has ended, the page offers a run on the new runtime
 * (the debugger with that runtime selected), a rebuild, and the way back to
 * where the build was started (`from`).
 */
export function BuildPage() {
  const { buildId } = useParams({ strict: false }) as { buildId?: string };
  const router = useRouter();
  const from = fromOf(useSearch({ strict: false }));
  const target = returnTarget(from);
  const { build, log, error } = useBuildLog(buildId);
  const buildDenied = useDenial('runtime.build');
  const cancel = useMutation({ mutationFn: () => api.cancelRuntimeBuild(buildId ?? '') });
  const running = !!build && !finished(build.state);
  const now = useNow(running ? 1000 : undefined);
  const outcome = build ? buildOutcome(build) : undefined;
  const elapsed = build ? buildElapsed(build, now) : undefined;
  const chip = build ? buildChip(build) : undefined;
  const missing = error instanceof ApiError && error.status === 404;
  const remembered = buildId ? recallBuild(browserStorage(), buildId) : undefined;
  const title = target ? setupTitle(target, getWorkspace().configPath) : undefined;
  // Selecting the runtime is all this does: the debugger shows it beside Run
  // and the open draft stays as it is.
  const runOn = (name: string) => {
    updateWorkspace({ runtime: name });
    router.history.push(target?.debugger ? target.href : '/');
  };
  const back = target && (
    <button type="button" className="od-btn od-btn--sm" onClick={() => router.history.push(target.href)}>
      <Icon name="arrow-left" size={14} /><span>{target.label}</span>
    </button>
  );

  return (
    <>
      <header className="od-wbhead">
        <Crumbs items={[{ label: 'Runtimes', to: '/runtimes' }, { label: build ? `Build ${build.name} ${build.collector_version}` : 'Build' }]} />
        {chip && <StatePill chip={chip} />}
        <span className="rt-grow" />
        {running && (
          <button type="button" className="od-btn" disabled={cancel.isPending || !!buildDenied} title={buildDenied} onClick={() => cancel.mutate()}>
            <Icon name="square" size={16} /><span>{cancel.isPending ? 'Cancelling…' : 'Cancel build'}</span>
          </button>
        )}
        {build && finished(build.state) && (
          <button type="button" className="od-btn" disabled={!!buildDenied} title={buildDenied ?? 'Open the builder with this build\'s name and manifest'}
            onClick={() => router.history.push(rebuildHref(build.id, from))}>
            <Icon name="rotate-ccw" size={16} /><span>Rebuild</span>
          </button>
        )}
      </header>

      {outcome ? (
        <div className={`od-callout od-callout--${outcome.tone} od-callout--bar`} role={outcome.tone === 'fail' ? 'alert' : 'status'}>
          <Icon name={outcome.tone === 'pass' ? 'circle-check' : outcome.tone === 'fail' ? 'circle-x' : 'triangle-alert'} size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{outcome.title}</span>
            <div className="od-callout__body">{outcome.body}</div>
          </div>
          <div className="od-callout__actions">
            {build?.state === 'done' && (
              <button type="button" className="od-btn od-btn--primary od-btn--sm" onClick={() => runOn(build.name)}>
                <Icon name="play" size={14} /><span>Run on {build.name}</span>
              </button>
            )}
            {!target && <Link className="od-btn od-btn--sm" to="/runtimes"><span>Runtimes</span></Link>}
            {back}
          </div>
        </div>
      ) : running ? (
        <div className="od-callout od-callout--info od-callout--bar" role="status">
          <Icon name="info" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{build?.state === 'queued' ? 'Waiting for a builder' : 'Builds keep running if you leave'}</span>
            <div className="od-callout__body">
              Follow it again from Runtimes.
              {title ? <> When it finishes, run on it or return to your experiment and choose the runtime beside Run.</> : <> When it finishes, run on it from here or choose the runtime beside Run in the debugger.</>}
            </div>
          </div>
          <div className="od-callout__actions">{back}</div>
        </div>
      ) : null}

      {error != null && (
        <div className="od-callout od-callout--fail od-callout--bar" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{missing ? 'This build is not available' : 'The build cannot be read'}</span>
            <div className="od-callout__body">
              {missing ? 'The server keeps the last 100 builds, and shows you your own; this one is older, was removed, or belongs to someone else. Built runtimes stay on the Runtimes page.' : problemOf(error, true).message}
            </div>
          </div>
          <div className="od-callout__actions"><Link className="od-btn od-btn--sm" to="/runtimes"><span>Runtimes</span></Link></div>
        </div>
      )}
      {cancel.error && <div className="od-callout od-callout--fail od-callout--bar" role="alert"><Icon name="circle-x" size={16} /><div className="od-callout__main"><span className="od-callout__title">Could not cancel</span><div className="od-callout__body">{problemOf(cancel.error, true).message}</div></div></div>}

      <div className="od-page">
        {!build && error == null && <p className="od-muted" role="status">Loading the build…</p>}
        {build && (
          <div className="rt-build">
            <div className="rt-build__side">
              <section className="od-card" aria-label="Build">
                <div className="od-card__head">
                  <h2 className="od-card__title">Build</h2>
                  <span className="rt-grow" />
                  {elapsed && <span className="od-subtle od-mono rt-note">{elapsed} {running ? 'elapsed' : 'taken'}</span>}
                </div>
                <div className="od-card__body rt-diag">
                  {running && (
                    <div className="od-progress od-progress--indet" role="progressbar" aria-label="Build in progress" aria-valuetext={chip?.label}><span /></div>
                  )}
                  <dl className="od-kv">
                    <dt>Runtime</dt><dd className="od-mono">{build.name}</dd>
                    <dt>Collector release</dt>
                    <dd>{build.collector_version}{!build.verified && <> <span className="od-chip od-chip--warn" title={UNVERIFIED}><Icon name="triangle-alert" size={12} />Unverified</span></>}</dd>
                    <dt>Output</dt><dd>runtime binary, run on the pool workers</dd>
                    {build.reused && (<><dt>Reuse</dt><dd>an identical earlier build&apos;s binary</dd></>)}
                    {build.image && (<><dt>Image</dt><dd className="od-mono">{build.image}</dd></>)}
                    {build.export_error && (<><dt>Image export</dt><dd>failed: {build.export_error}</dd></>)}
                    {build.log_lines > 0 && (<><dt>Log lines</dt><dd className="od-mono">{build.log_lines}</dd></>)}
                  </dl>
                  {(build.warnings ?? []).map((w, i) => (
                    <div key={i} className="od-callout od-callout--warn"><Icon name="triangle-alert" size={16} /><div className="od-callout__main"><div className="od-callout__body">{w}</div></div></div>
                  ))}
                </div>
              </section>
            </div>

            <section className="od-card rt-build__log" aria-label="Build log" style={{ overflow: 'hidden', alignSelf: 'start' }}>
              <LogHead lines={log.lines} skipped={log.skipped} />
              <LogBody lines={log.lines} skipped={log.skipped} running={running} queued={build.state === 'queued'} />
            </section>

            <div className="rt-build__manifest">
              <ManifestCard manifest={remembered?.manifest ?? build?.manifest_yaml} />
            </div>
          </div>
        )}
      </div>
    </>
  );
}

function ManifestCard({ manifest }: { manifest: string | undefined }) {
  if (!manifest) {
    return (
      <section className="od-card" aria-label="Manifest">
        <div className="od-card__head"><h2 className="od-card__title">Manifest</h2></div>
        <div className="od-card__body od-muted rt-note">
          The server did not return this build&apos;s manifest.
        </div>
      </section>
    );
  }
  const lines = manifest.replace(/\n$/, '').split('\n');
  return (
    <section className="od-card" aria-label="Manifest" style={{ overflow: 'hidden' }}>
      <div className="od-card__head">
        <h2 className="od-card__title">Manifest</h2>
        <span className="od-subtle rt-note">as sent · read-only</span>
      </div>
      <pre className="od-code od-code--flat">
        {lines.slice(0, MANIFEST_LINES).map((text, i) => (
          <div key={i} className="od-code__line"><span className="od-code__gutter" /><span className="od-code__ln">{i + 1}</span><span>{text}</span></div>
        ))}
      </pre>
      {lines.length > MANIFEST_LINES && <div className="od-pager"><span>{lines.length - MANIFEST_LINES} more lines are not shown.</span></div>}
    </section>
  );
}

function LogHead({ lines, skipped }: { lines: string[]; skipped: number }) {
  const [copied, setCopied] = useState<string>();
  const copy = () => {
    try {
      navigator.clipboard.writeText(logText(lines, skipped)).then(() => setCopied('Copied'), () => setCopied('Not copied'));
    } catch {
      setCopied('Not copied');
    }
  };
  return (
    <div className="od-card__head">
      <h2 className="od-card__title">Log</h2>
      <span className="rt-grow" />
      {copied && <span className="od-subtle rt-note" role="status">{copied}</span>}
      <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={lines.length === 0} onClick={copy}>
        <Icon name="copy" size={14} /><span>Copy diagnostics</span>
      </button>
    </div>
  );
}

function LogBody({ lines, skipped, running, queued }: { lines: string[]; skipped: number; running: boolean; queued: boolean }) {
  const ref = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  // Follow the end of the log while the reader is at the end of it.
  useEffect(() => {
    const el = ref.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [lines.length]);
  const numbered = numberedLog(lines, skipped);
  return (
    <>
      {skipped > 0 && <div className="od-pager"><span>{skipped} earlier log lines are not kept.</span></div>}
      <pre
        ref={ref}
        className="od-code rt-log"
        role="log"
        aria-label="Build log lines"
        tabIndex={0}
        onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
        }}
      >
        {numbered.length === 0 && <div className="od-code__line"><span className="od-code__gutter" /><span className="od-code__ln" /><span className="od-subtle">{queued ? 'Queued: a builder has not taken this build yet.' : running ? 'Waiting for the first log line…' : 'The build wrote no log.'}</span></div>}
        {numbered.map((l) => (
          <div key={l.n} className="od-code__line"><span className="od-code__gutter" /><span className="od-code__ln">{l.n}</span><span>{l.text}</span></div>
        ))}
      </pre>
    </>
  );
}
