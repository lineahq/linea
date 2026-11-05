-- M6 step 6c: the component types each Collector build provides, for the
-- compatibility preflight. Agents report a hash of their build; the full list
-- is requested once per unknown hash and shared by every agent of that build.
CREATE TABLE fleet.component_sets (
    hash text COLLATE "C" PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{2,128}$'),
    components jsonb NOT NULL CHECK (jsonb_typeof(components) = 'object' AND octet_length(components::text) <= 65536),
    first_seen_at timestamptz NOT NULL
);

ALTER TABLE fleet.agents ADD COLUMN available_components_hash text
    CHECK (available_components_hash ~ '^[0-9a-f]{2,128}$');
-- Retention keeps sets that an agent still references.
CREATE INDEX agents_available_components_idx ON fleet.agents (available_components_hash)
    WHERE available_components_hash IS NOT NULL;

-- An incompatible target is never released: its build lacks a component type
-- the rollout's version uses.
ALTER TABLE fleet.rollout_targets DROP CONSTRAINT rollout_targets_outcome_check;
ALTER TABLE fleet.rollout_targets DROP CONSTRAINT rollout_targets_check;
ALTER TABLE fleet.rollout_targets ADD CONSTRAINT rollout_targets_outcome_check
    CHECK (outcome IN ('pending', 'released', 'baking', 'offline', 'succeeded', 'failed', 'unhealthy', 'timed_out', 'unmatched', 'incompatible'));
ALTER TABLE fleet.rollout_targets ADD CONSTRAINT rollout_targets_released_check
    CHECK ((outcome IN ('pending', 'incompatible')) = (released_at IS NULL));

REVOKE ALL ON fleet.component_sets FROM PUBLIC;
