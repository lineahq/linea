import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { stringify } from 'yaml';
import { api } from '../api/client';
import type { Inspection } from '../api/types';
import type { LayoutNode } from '../graph/layout';
import { useWorkspace } from '../workspace/store';
import { Icon } from '../ui/Icon';
import { asWritten } from './componentConfig';

/**
 * ConfigurationView shows a component's configuration two ways: effective
 * (defaults applied, secrets redacted, from the validated draft) and as
 * written in the configuration text. With a run selected, "as written" reads
 * the configuration that run was admitted with; otherwise the draft.
 */
export function ConfigurationView({ node, inspection, runId }: { node: LayoutNode; inspection?: Inspection; runId?: string }) {
  const c = node.component;
  const ws = useWorkspace();
  const inputs = useQuery({ queryKey: ['run-inputs', runId], queryFn: () => api.runInputs(runId!), enabled: !!runId, staleTime: Infinity, retry: false });
  const [how, setHow] = useState<'effective' | 'written'>('effective');
  const section = inspection?.effective?.[`${c.kind}s`] as Record<string, unknown> | undefined;
  const effective = section?.[c.id];
  const text = runId ? inputs.data?.config.text : ws.configYAML;
  const written = text === undefined ? undefined : asWritten(text, c.kind, c.id);

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '10px 12px', borderBottom: '1px solid var(--border-subtle)' }}>
        <Icon name="info" size={14} />
        <span className="od-muted" style={{ fontSize: 12.5 }}>
          {how === 'effective'
            ? runId
              ? 'The open configuration: defaults applied, secrets redacted. The run used the configuration listed under Sources.'
              : 'As the next run will see it: defaults applied, secrets redacted.'
            : runId
              ? 'Exactly as written in the configuration this run was admitted with.'
              : 'Exactly as written in the open configuration.'}
        </span>
        <span style={{ flex: 1 }} />
        <div className="od-seg" role="group" aria-label="Configuration form">
          <button type="button" aria-pressed={how === 'effective'} onClick={() => setHow('effective')}>Effective</button>
          <button type="button" aria-pressed={how === 'written'} onClick={() => setHow('written')}>As written</button>
        </div>
      </div>
      {how === 'effective' ? (
        effective === undefined ? (
          <div className="od-muted" style={{ padding: 14, fontSize: 12.5 }} role="status">
            {inspection ? 'The effective configuration is not available for this component in the open configuration.' : 'The effective configuration appears when the configuration is valid.'}
          </div>
        ) : (
          <pre className="od-code od-code--flat" style={{ margin: 0, padding: '8px 14px' }}>{stringify(effective, { lineWidth: 0 }).trimEnd() || '{}'}</pre>
        )
      ) : written === undefined ? (
        <div className="od-muted" style={{ padding: 14, fontSize: 12.5 }} role="status">
          {runId && inputs.isPending ? 'Loading the run’s configuration…' : `${c.id} is not in the configuration text${text === undefined ? ' that is available' : ' or the text does not parse'}.`}
        </div>
      ) : (
        <pre className="od-code od-code--flat" style={{ margin: 0, padding: '8px 14px' }}>
          {written || '# declared without settings: defaults apply'}
        </pre>
      )}
    </div>
  );
}
