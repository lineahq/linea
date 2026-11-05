/** Destination is one of the top-level places in the rail. */
export type Destination = 'debugger' | 'playground' | 'capture' | 'fleet' | 'runtimes' | 'saved' | 'audit' | 'access';

/** destinationOf names the rail destination a path belongs to. */
export function destinationOf(pathname: string): Destination {
  const first = pathname.split('/').filter(Boolean)[0] ?? '';
  if (first === 'playground') return 'playground';
  if (first === 'captures') return 'capture';
  // /collectors/... are the old Live capture paths; they redirect to Fleet or Live capture.
  if (first === 'fleet' || first === 'collectors') return 'fleet';
  if (first === 'runtimes') return 'runtimes';
  if (first === 'saved') return 'saved';
  if (first === 'audit') return 'audit';
  if (first === 'access') return 'access';
  return 'debugger';
}

/** activeCaptureCount counts capture sessions that have not finished. */
export function activeCaptureCount(sessions: { terminal: boolean }[] | undefined): number {
  return (sessions ?? []).filter((s) => !s.terminal).length;
}

/** activeBuildCount counts runtime builds that have not finished. */
export function activeBuildCount(builds: { state: string }[] | undefined, finished: (state: string) => boolean): number {
  return (builds ?? []).filter((b) => !finished(b.state)).length;
}
