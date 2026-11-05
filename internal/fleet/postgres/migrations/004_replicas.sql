-- v2 state model: presence follows replica liveness, events are an outbox
-- read by (xid, id), and singleton jobs run under a leader lease.
CREATE TABLE fleet.replicas (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    started_at timestamptz NOT NULL DEFAULT now(),
    heartbeat_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE fleet.leases (
    name text COLLATE "C" PRIMARY KEY CHECK (name ~ '^[a-z0-9-]{1,64}$'),
    holder text COLLATE "C" NOT NULL CHECK (holder ~ '^[0-9a-f]{32}$'),
    expires_at timestamptz NOT NULL
);

-- Existing agents become disconnected records without an owner.
ALTER TABLE fleet.agents RENAME COLUMN last_report_at TO last_seen_at;
ALTER TABLE fleet.agents
    ADD COLUMN owner_replica text COLLATE "C",
    ADD COLUMN first_seen_at timestamptz,
    ADD COLUMN credential_id text COLLATE "C" CHECK (credential_id ~ '^[0-9a-f]{32}$'),
    ADD COLUMN identity_conflict boolean NOT NULL DEFAULT false,
    ADD COLUMN takeover_count integer NOT NULL DEFAULT 0 CHECK (takeover_count >= 0),
    ADD COLUMN takeover_at timestamptz;
UPDATE fleet.agents SET first_seen_at = connected_at, disconnected_at = COALESCE(disconnected_at, now());
ALTER TABLE fleet.agents
    ALTER COLUMN first_seen_at SET DEFAULT now(),
    ALTER COLUMN first_seen_at SET NOT NULL,
    DROP COLUMN presence;
CREATE INDEX agents_owner_idx ON fleet.agents (owner_replica, id) WHERE owner_replica IS NOT NULL;

-- Readers return only events from transactions below the snapshot xmin.
ALTER TABLE fleet.events
    ADD COLUMN xid xid8 NOT NULL DEFAULT pg_current_xact_id(),
    ADD COLUMN detail jsonb CHECK (detail IS NULL OR octet_length(detail::text) <= 1024);
CREATE INDEX events_stream_idx ON fleet.events (xid, id);

-- The hot high-water row is gone; the floor moves only when the leader trims.
-- The new epoch expires every v1 cursor once.
ALTER TABLE fleet.metadata RENAME COLUMN event_floor TO event_floor_id;
ALTER TABLE fleet.metadata
    DROP COLUMN event_highwater,
    ADD COLUMN event_floor_xid xid8 NOT NULL DEFAULT '0',
    ADD COLUMN system_identifier text CHECK (system_identifier ~ '^[0-9]{1,20}$');
UPDATE fleet.metadata SET event_epoch = md5(random()::text || clock_timestamp()::text);

REVOKE ALL ON fleet.replicas, fleet.leases FROM PUBLIC;
