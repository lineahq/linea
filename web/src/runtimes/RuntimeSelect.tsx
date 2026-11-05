import { useQuery } from '@tanstack/react-query';
import { Link, useLocation } from '@tanstack/react-router';
import { api } from '../api/client';
import { updateWorkspace } from '../workspace/store';
import { problemOf, runtimeOptions, UNVERIFIED } from './runtimes';

export const runtimesQuery = { queryKey: ['runtimes'], queryFn: api.runtimes, retry: false } as const;
/** defaultRuntimeQuery reads the team's default runtime: what a request that names no runtime runs on. */
export const defaultRuntimeQuery = { queryKey: ['runtime-default'], queryFn: api.defaultRuntime, retry: false } as const;
export const supportQuery = { queryKey: ['collector-versions'], queryFn: api.collectorSupport, staleTime: Infinity } as const;

/**
 * RuntimeSelect is the one place a runtime is chosen: beside Run, it
 * names the runtime the next run, the validation and the previews use. Its
 * first option, "Default", names no runtime: the team's default answers,
 * and the option says which; built-in stays a choice of its own. An
 * unverified runtime shows its badge whenever it is selected. "Runtimes…"
 * goes to the Runtimes destination, which lists and builds runtimes and sets
 * the team's default but never chooses the runtime of this workspace; `from`
 * is where the user returns to choose it.
 */
export function RuntimeSelect({ selected }: { selected: string }) {
  const location = useLocation();
  const runtimes = useQuery(runtimesQuery);
  const team = useQuery(defaultRuntimeQuery);
  const support = useQuery(supportQuery);
  const options = runtimeOptions(runtimes.data, selected, support.data?.built_in, team.data?.name);
  const current = options.find((o) => o.value === selected);
  const unavailable = runtimes.error ? problemOf(runtimes.error).message : undefined;
  return (
    <div className="runtime-select">
      <select
        aria-label="Runtime"
        value={selected}
        title={unavailable ?? 'The debug Collector runs, validation and previews use. Default is the runtime the team chose.'}
        onChange={(e) => updateWorkspace({ runtime: e.target.value || undefined })}
      >
        {options.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
      </select>
      {current?.unverified && <span className="badge unverified" title={UNVERIFIED}>unverified</span>}
      {current?.missing && !runtimes.isPending && <span className="error" role="alert">Runtime {selected} is not available on this server; choose another or add it.</span>}
      <Link to="/runtimes" search={{ from: `${location.pathname}${location.searchStr}` }} title="List, build and remove debug Collectors, and choose the team's default">Runtimes…</Link>
    </div>
  );
}
