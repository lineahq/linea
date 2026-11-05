-- Run retention.
--
-- origin_run is the run a revision was made from, copied out of origin so
-- that retention can protect a run an artifact's head revision still
-- references. Revisions written before this migration get it here.
ALTER TABLE revisions ADD COLUMN origin_run text;
UPDATE revisions SET origin_run = convert_from(origin, 'UTF8')::jsonb ->> 'run_id'
    WHERE origin IS NOT NULL;
CREATE INDEX revisions_origin_run_idx ON revisions (origin_run) WHERE origin_run IS NOT NULL;

-- Retention evicts the oldest terminal runs that are not kept.
CREATE INDEX runs_evictable_idx ON runs (ordinal) WHERE terminal AND NOT kept;
