import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import { problemOf, referenceProblem } from './runtimes';

/**
 * The small runtime forms shared by the runtimes dialog and the Runtimes
 * page: push an image, import an image. None of
 * them chooses a runtime; that stays with the selector beside Run.
 */

/** PushForm pushes a runtime's image with the server user's Docker credentials. */
export function PushForm({ name, onDone }: { name: string; onDone(): void }) {
  const qc = useQueryClient();
  const [target, setTarget] = useState('');
  const [pushed, setPushed] = useState<string>();
  const push = useMutation({
    mutationFn: (t: string) => api.pushRuntime(name, t),
    onSuccess: (_, t) => { setPushed(t); void qc.invalidateQueries({ queryKey: ['runtimes'] }); },
  });
  const problem = target ? referenceProblem(target, true) : undefined;
  return (
    <form className="push-form" aria-label={`Push ${name}`} onSubmit={(e) => { e.preventDefault(); if (!referenceProblem(target, true)) push.mutate(target.trim()); }}>
      <input type="text" aria-label="Push to" placeholder="registry.example.com/team/runtime:tag" value={target} onChange={(e) => { setTarget(e.target.value); setPushed(undefined); }} />
      <button type="submit" className="primary" disabled={!target || !!problem || push.isPending}>{push.isPending ? 'Pushing…' : 'Push'}</button>
      <button type="button" onClick={onDone}>{pushed ? 'Done' : 'Cancel'}</button>
      {problem && <p className="error small">{problem}</p>}
      {push.error && <p className="error small">{problemOf(push.error).message}</p>}
      {pushed && <p className="note small">Pushed to <code>{pushed}</code>. Import it elsewhere with <code>linea runtime import {pushed} --pull</code>.</p>}
    </form>
  );
}

/** ImportForm accepts a runtime image by reference, pulling it first when asked. */
export function ImportForm() {
  const qc = useQueryClient();
  const [ref, setRef] = useState('');
  const [pull, setPull] = useState(true);
  const imp = useMutation({
    mutationFn: () => api.importRuntime(ref.trim(), pull),
    onSuccess: () => { setRef(''); void qc.invalidateQueries({ queryKey: ['runtimes'] }); },
  });
  const problem = ref ? referenceProblem(ref) : undefined;
  return (
    <form className="import-form" aria-label="Import runtime" onSubmit={(e) => { e.preventDefault(); if (!referenceProblem(ref)) imp.mutate(); }}>
      <input type="text" aria-label="Image reference" placeholder="registry.example.com/team/runtime:tag" value={ref} onChange={(e) => { setRef(e.target.value); imp.reset(); }} />
      <label className="small"><input type="checkbox" checked={pull} onChange={(e) => setPull(e.target.checked)} /> Pull from the registry</label>
      <button type="submit" disabled={!ref || !!problem || imp.isPending}>{imp.isPending ? 'Importing…' : 'Import'}</button>
      {problem && <p className="error small">{problem}</p>}
      {imp.error && <p className="error small">{problemOf(imp.error).message}</p>}
      {imp.data && <p className="note small">Imported <code>{imp.data.name}</code> (Collector {imp.data.collector_version}).</p>}
    </form>
  );
}
