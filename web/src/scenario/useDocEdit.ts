import { useState } from 'react';
import { getWorkspace, updateWorkspace } from '../workspace/store';
import { editScenario, readScenario, type Edit } from './scenarioDoc';

/**
 * useDocEdit reads the document's form model and applies edits to its YAML,
 * which stays the source of truth, so the YAML tab and the forms always
 * agree and comments survive. Edits apply to the document as it
 * is when they run, so a buffered editor can commit late.
 */
export function useDocEdit(yaml: string) {
  const [problem, setProblem] = useState<string>();
  const read = readScenario(yaml);
  const apply = (e: Edit) => {
    const r = editScenario(getWorkspace().scenarioYAML, e);
    if ('error' in r) {
      setProblem(r.error);
      return false;
    }
    setProblem(undefined);
    updateWorkspace({ scenarioYAML: r.yaml });
    return true;
  };
  return { read, apply, problem };
}
