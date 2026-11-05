-- Paged and filtered run lists.
--
-- scenario, runtime_name and result are what a run list filters on, written
-- by the platform with each publication. runtime_name is the runtime a run
-- executed on; runtime (002) is the queue key of the workers that may take
-- it. indexed is false for runs stored before this migration until the
-- platform indexes them; their filter columns are NULL meanwhile.
-- kept protects a run from retention.
ALTER TABLE runs
    ADD COLUMN scenario     text,
    ADD COLUMN runtime_name text,
    ADD COLUMN result       text,
    ADD COLUMN indexed      boolean NOT NULL DEFAULT false,
    ADD COLUMN kept         boolean NOT NULL DEFAULT false;

ALTER TABLE runs
    ADD CONSTRAINT runs_index_bounds CHECK (
        octet_length(coalesce(scenario, '')) <= 256 AND
        octet_length(coalesce(runtime_name, '')) <= 256 AND
        octet_length(coalesce(result, '')) <= 64);

-- Newest first per filter; runs_project_idx (001) serves the unfiltered list.
CREATE INDEX runs_scenario_idx ON runs (project_id, scenario, ordinal DESC) WHERE scenario IS NOT NULL;
CREATE INDEX runs_runtime_name_idx ON runs (project_id, runtime_name, ordinal DESC) WHERE runtime_name IS NOT NULL;
CREATE INDEX runs_result_idx ON runs (project_id, result, ordinal DESC) WHERE result IS NOT NULL;
CREATE INDEX runs_unindexed_idx ON runs (ordinal) WHERE NOT indexed;
