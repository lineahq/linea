-- Shared instances.

-- released counts the attempts a draining instance handed back unfinished;
-- they do not count toward a run's attempt limit.
ALTER TABLE runs ADD COLUMN released integer NOT NULL DEFAULT 0 CHECK (released >= 0);
--
-- changes is the change feed: every write another instance
-- must see records a row in its own transaction, by trigger, and notifies
-- linea_debugger with the schema name as payload. Readers return rows of
-- finished transactions only (xid below the snapshot's xmin), ordered by
-- (xid, id). change_floor is the newest trimmed position; a cursor below it
-- has expired.
CREATE TABLE changes (
    id   bigserial   PRIMARY KEY,
    xid  xid8        NOT NULL DEFAULT pg_current_xact_id(),
    kind text        NOT NULL CHECK (kind IN ('run', 'events', 'evaluation', 'suite', 'deletion', 'job')),
    ref  text        NOT NULL,
    at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX changes_position_idx ON changes (xid, id);

CREATE TABLE change_floor (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    xid       xid8    NOT NULL DEFAULT '0',
    id        bigint  NOT NULL DEFAULT 0
);
INSERT INTO change_floor DEFAULT VALUES;

-- run_events keeps the live events of unfinished runs in batches, for
-- watchers on other instances. A later attempt replaces an earlier one; a
-- terminal publication deletes them (the evidence archive keeps every event).
CREATE TABLE run_events (
    run_id    text    NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    attempt   integer NOT NULL CHECK (attempt > 0),
    first_seq integer NOT NULL CHECK (first_seq > 0),
    last_seq  integer NOT NULL CHECK (last_seq >= first_seq),
    payload   bytea   NOT NULL,
    PRIMARY KEY (run_id, attempt, first_seq)
);

-- jobs are interactive requests (inspection, OTTL preview) that an instance
-- without a free worker of the runtime posts for any instance with one. They
-- keep no history: the requester deletes a job once answered, and leader
-- work trims those whose requester gave up.
CREATE TABLE jobs (
    id         text        PRIMARY KEY,
    runtime    text        NOT NULL,
    kind       text        NOT NULL CHECK (kind IN ('inspect', 'ottl')),
    input      bytea       NOT NULL,
    state      text        NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'claimed', 'done')),
    worker     text        NOT NULL DEFAULT '',
    result     bytea,
    error      bytea,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_queued_idx ON jobs (runtime, created_at) WHERE state = 'queued';

CREATE FUNCTION record_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    k text := TG_ARGV[0];
    r text;
BEGIN
    IF TG_TABLE_NAME = 'runs' THEN
        IF TG_OP = 'UPDATE' AND NEW.generation = OLD.generation AND NEW.cancel_requested = OLD.cancel_requested THEN
            RETURN NULL;
        END IF;
        r := NEW.id;
    ELSIF TG_TABLE_NAME IN ('evaluations', 'run_events') THEN
        r := NEW.run_id;
    ELSE
        r := NEW.id;
    END IF;
    INSERT INTO changes (kind, ref) VALUES (k, r);
    PERFORM pg_notify('linea_debugger', TG_TABLE_SCHEMA);
    RETURN NULL;
END $$;

CREATE TRIGGER runs_change AFTER INSERT OR UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION record_change('run');
CREATE TRIGGER evaluations_change AFTER INSERT ON evaluations
    FOR EACH ROW EXECUTE FUNCTION record_change('evaluation');
CREATE TRIGGER suites_change AFTER INSERT ON suites
    FOR EACH ROW EXECUTE FUNCTION record_change('suite');
CREATE TRIGGER tombstones_change AFTER INSERT ON tombstones
    FOR EACH ROW EXECUTE FUNCTION record_change('deletion');
CREATE TRIGGER run_events_change AFTER INSERT ON run_events
    FOR EACH ROW EXECUTE FUNCTION record_change('events');
-- A claim changes nothing a requester or another claimer waits for.
CREATE TRIGGER jobs_change AFTER INSERT OR UPDATE OF state ON jobs
    FOR EACH ROW WHEN (NEW.state <> 'claimed') EXECUTE FUNCTION record_change('job');

-- logins are OIDC sign-ins in progress, so any instance completes a sign-in
-- another started. The key is the SHA-256 of the sign-in's state.
CREATE TABLE logins (
    hash       text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    verifier   text        NOT NULL,
    nonce      text        NOT NULL,
    return_to  text        NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX logins_expires_idx ON logins (expires_at);

-- workers are the workers connected to each instance, as the instance last
-- recorded them, so every instance counts all of them. An instance renews its
-- rows while it runs; those of one that stopped expire and leader work
-- deletes them.
CREATE TABLE workers (
    instance   text        NOT NULL,
    id         text        NOT NULL,
    identity   text        NOT NULL,
    capacity   integer     NOT NULL CHECK (capacity > 0),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (instance, id)
);
CREATE INDEX workers_expires_idx ON workers (expires_at);
