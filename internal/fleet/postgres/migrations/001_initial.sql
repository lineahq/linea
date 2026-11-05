CREATE SCHEMA fleet;
REVOKE ALL ON SCHEMA fleet FROM PUBLIC;
CREATE TABLE fleet.schema_version (
    version integer PRIMARY KEY CHECK (version > 0),
    checksum text NOT NULL,
    installed_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE fleet.metadata (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    server_id text NOT NULL CHECK (length(server_id) BETWEEN 1 AND 128),
    event_epoch text NOT NULL CHECK (event_epoch ~ '^[0-9a-f]{32}$'),
    event_floor bigint NOT NULL DEFAULT 0 CHECK (event_floor >= 0),
    event_highwater bigint NOT NULL DEFAULT 0 CHECK (event_highwater >= event_floor)
);
CREATE TABLE fleet.agents (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    generation bigint NOT NULL CHECK (generation > 0),
    presence text NOT NULL CHECK (presence IN ('connected', 'disconnected', 'unknown')),
    connected_at timestamptz NOT NULL,
    disconnected_at timestamptz,
    last_report_at timestamptz,
    capabilities numeric(20,0) NOT NULL DEFAULT 0 CHECK (capabilities BETWEEN 0 AND 18446744073709551615),
    healthy boolean,
    health_observed_at timestamptz,
    CHECK ((healthy IS NULL) = (health_observed_at IS NULL))
);
CREATE INDEX agents_presence_id_idx ON fleet.agents (presence, id);
CREATE TABLE fleet.credentials (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    role text NOT NULL CHECK (role IN ('admin', 'reader', 'agent')),
    agent_id text CHECK (agent_id ~ '^[0-9a-f]{32}$'),
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    revoked_at timestamptz,
    CHECK ((role = 'agent' AND agent_id IS NOT NULL) OR (role <> 'agent' AND agent_id IS NULL))
);
CREATE TABLE fleet.groups (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    revision bigint NOT NULL CHECK (revision > 0)
);
CREATE TABLE fleet.group_members (
    group_id text NOT NULL REFERENCES fleet.groups(id) ON DELETE CASCADE,
    agent_id text NOT NULL REFERENCES fleet.agents(id),
    PRIMARY KEY (group_id, agent_id)
);
CREATE INDEX group_members_agent_id_idx ON fleet.group_members (agent_id);
CREATE TABLE fleet.events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL,
    subject_id text NOT NULL,
    actor_id text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON ALL TABLES IN SCHEMA fleet FROM PUBLIC;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA fleet FROM PUBLIC;
