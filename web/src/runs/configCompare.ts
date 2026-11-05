import type { ArtifactMetadata, ComparisonRequest, Run, RunInputs, ScenarioFromRun } from '../api/types';

/** The configuration a run's input is run with beside the run's own. */
export type CompareTarget =
  | { kind: 'draft'; yaml: string; label?: string }
  | { kind: 'saved'; artifact: ArtifactMetadata };

/**
 * configComparisonRequest runs a run's recorded configuration (before) and
 * another one (after) on the run's input, as one comparison: both runs are
 * admitted together on the run's runtime. The input is the self-contained
 * scenario derived from the run (as a suite rerun uses), so a quick run and
 * a scenario that read files beside it run the same way.
 */
export function configComparisonRequest(run: Run, inputs: RunInputs, scenario: ScenarioFromRun, after: CompareTarget): ComparisonRequest {
  const rt = run.report?.runtime;
  return {
    before: { config_yaml: inputs.config.text, ...(inputs.config.label ? { config_label: inputs.config.label } : {}) },
    after: after.kind === 'saved'
      ? { config_ref: { artifact_id: after.artifact.id, revision_id: after.artifact.head_revision_id }, config_label: after.artifact.label }
      : { config_yaml: after.yaml, ...(after.label ? { config_label: after.label } : {}) },
    scenario_yaml: scenario.yaml,
    scenario_label: inputs.scenario?.label || `run ${run.id.replace(/^run-/, '').slice(0, 8)}`,
    ...(rt && rt.kind !== 'built_in' && rt.name ? { runtime: rt.name } : {}),
  };
}
