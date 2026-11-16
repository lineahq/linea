-- The administrative record. Changes to credentials, groups, configs,
-- assignments, rollouts and plugin lifecycle are written here in the same
-- transaction as the change and as its event. fleet.events is trimmed by
-- count, so an agent reconnect storm can evict them there; here they are
-- kept by time (400 days) with a row cap as a safety bound, and agent
-- activity never writes.
CREATE TABLE fleet.audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL,
    subject_id text NOT NULL,
    actor_id text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    detail jsonb,
    acting_for jsonb
);
CREATE INDEX audit_occurred_idx ON fleet.audit (occurred_at, id);
CREATE INDEX audit_kind_idx ON fleet.audit (kind, id);
CREATE INDEX audit_actor_idx ON fleet.audit (actor_id, id);
