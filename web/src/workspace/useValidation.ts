import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { useDebounced } from '../useDebounced';
import type { WorkspaceState } from './store';
import { setupInUse } from '../input/quickInput';
import { runtimeField } from '../runtimes/runtimes';

/**
 * useValidation validates the configuration text shortly after each edit
 * on the runtime chosen beside Run (the built-in
 * registry by default). The document supplies
 * ${env:NAME} values and mock fixtures; invalid document text only produces
 * a warning.
 */
export function useValidation(ws: WorkspaceState) {
  const scenario = setupInUse(ws.scenarioYAML);
  const req = useDebounced(
    {
      config_yaml: ws.configYAML,
      scenario_yaml: scenario ? ws.scenarioYAML : undefined,
      ...runtimeField(ws.runtime),
    },
    350,
  );
  return useQuery({
    queryKey: ['validate', req],
    queryFn: ({ signal }) => api.validate(req, signal),
    enabled: req.config_yaml.trim() !== '',
    placeholderData: keepPreviousData,
  });
}
