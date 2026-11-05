-- M6 step 6b: every change to what agents run goes through a rollout. An
-- assignment created by a rollout has no active version until it completes;
-- until then only the rollout's released targets receive its layer. The
-- controller keeps no state in memory: a leader change resumes from here.
ALTER TABLE fleet.config_assignments ALTER COLUMN version DROP NOT NULL;

-- Rollouts keep their assignment ID after the assignment is deleted, so they
-- reference it without a foreign key. from_version is NULL for a rollout
-- creating its assignment, to_version for one deleting it.
CREATE TABLE fleet.rollouts (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    assignment_id text COLLATE "C" NOT NULL CHECK (assignment_id ~ '^[0-9a-f]{32}$'),
    config_id text COLLATE "C" NOT NULL CHECK (config_id ~ '^[0-9a-f]{32}$'),
    from_version integer CHECK (from_version > 0),
    to_version integer CHECK (to_version > 0),
    state text NOT NULL CHECK (state IN ('running', 'paused', 'rolling_back', 'completed', 'rolled_back', 'cancelled')),
    current_wave smallint NOT NULL DEFAULT -1 CHECK (current_wave BETWEEN -1 AND 10),
    wave_count smallint NOT NULL CHECK (wave_count BETWEEN 0 AND 11),
    targets integer NOT NULL CHECK (targets BETWEEN 0 AND 20000),
    phase_started_at timestamptz,
    policy jsonb NOT NULL CHECK (jsonb_typeof(policy) = 'object' AND octet_length(policy::text) <= 2048),
    reason text CHECK (reason IN ('operator', 'threshold')),
    created_at timestamptz NOT NULL,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK (from_version IS NOT NULL OR to_version IS NOT NULL)
);
-- At most one active rollout per assignment.
CREATE UNIQUE INDEX rollouts_active_assignment_idx ON fleet.rollouts (assignment_id)
    WHERE state IN ('running', 'paused', 'rolling_back');
CREATE INDEX rollouts_finished_idx ON fleet.rollouts (updated_at, id)
    WHERE state IN ('completed', 'rolled_back', 'cancelled');

-- Targets are snapshotted at admission. Agents may be deleted by retention,
-- so agent_id has no foreign key either.
CREATE TABLE fleet.rollout_targets (
    rollout_id text COLLATE "C" NOT NULL REFERENCES fleet.rollouts(id) ON DELETE CASCADE,
    agent_id text COLLATE "C" NOT NULL CHECK (agent_id ~ '^[0-9a-f]{32}$'),
    wave smallint NOT NULL CHECK (wave BETWEEN 0 AND 10),
    released_at timestamptz,
    outcome text NOT NULL DEFAULT 'pending'
        CHECK (outcome IN ('pending', 'released', 'baking', 'offline', 'succeeded', 'failed', 'unhealthy', 'timed_out', 'unmatched')),
    observed_at timestamptz,
    accepted boolean NOT NULL DEFAULT false,
    active boolean NOT NULL DEFAULT true,
    PRIMARY KEY (rollout_id, agent_id),
    CHECK ((outcome = 'pending') = (released_at IS NULL))
);
-- An agent is in at most one active rollout, so failures are attributable.
CREATE UNIQUE INDEX rollout_targets_active_agent_idx ON fleet.rollout_targets (agent_id) WHERE active;
CREATE INDEX rollout_targets_progress_idx ON fleet.rollout_targets (rollout_id, wave, outcome);

REVOKE ALL ON fleet.rollouts, fleet.rollout_targets FROM PUBLIC;
