-- Run scheduling.
--
-- queue_state is NULL for a terminal run and for a run published without
-- admission. A queued run waits for a worker of its runtime; an assigned or
-- running one is held by worker until lease_expires_at, which the worker's
-- heartbeat renews. attempt counts the run's assignments.
ALTER TABLE runs
    ADD COLUMN queue_state      text        CHECK (queue_state IN ('queued', 'assigned', 'running')),
    ADD COLUMN runtime          text        NOT NULL DEFAULT '',
    ADD COLUMN priority         integer     NOT NULL DEFAULT 0,
    ADD COLUMN worker           text        NOT NULL DEFAULT '',
    ADD COLUMN attempt          integer     NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN cancel_requested boolean     NOT NULL DEFAULT false;

ALTER TABLE runs
    ADD CONSTRAINT runs_queue_unfinished CHECK (queue_state IS NULL OR NOT terminal),
    ADD CONSTRAINT runs_queue_lease CHECK (
        (coalesce(queue_state, '') IN ('assigned', 'running')) = (lease_expires_at IS NOT NULL AND worker <> ''));

-- The scheduler query: queued runs of one runtime in priority, then admission, order.
CREATE INDEX runs_queued_idx ON runs (runtime, priority, ordinal) WHERE queue_state = 'queued';
CREATE INDEX runs_leased_idx ON runs (worker) WHERE queue_state IN ('assigned', 'running');
CREATE INDEX runs_lease_expiry_idx ON runs (lease_expires_at) WHERE queue_state IN ('assigned', 'running');
