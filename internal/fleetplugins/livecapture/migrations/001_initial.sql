CREATE SCHEMA capture;
REVOKE ALL ON SCHEMA capture FROM PUBLIC;
CREATE TABLE capture.schema_version (
    version integer PRIMARY KEY CHECK (version > 0),
    checksum text NOT NULL CHECK (length(checksum)=64)
);
CREATE TABLE capture.metadata (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    plugin_id text NOT NULL CHECK(length(plugin_id) BETWEEN 1 AND 64),
    authority_id text NOT NULL CHECK(authority_id ~ '^[0-9a-f]{32}$')
);
CREATE TABLE capture.peers (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
    generation bigint NOT NULL CHECK (generation>0)
);
CREATE TABLE capture.operations (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    owner_id text COLLATE "C" NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
    state text NOT NULL CHECK (state IN ('starting','active','stopping','completed','stopped','failed','expired','interrupted')),
    reason text NOT NULL DEFAULT '' CHECK(length(reason)<=64),
    revision bigint NOT NULL CHECK (revision>0),
    expires_at timestamptz NOT NULL,
    channel_id text NOT NULL CHECK (channel_id ~ '^[0-9a-f]{32}$'),
    targets jsonb NOT NULL CHECK (jsonb_typeof(targets)='array' AND jsonb_array_length(targets) BETWEEN 1 AND 16)
);
CREATE TABLE capture.leases (
    peer_id text COLLATE "C" PRIMARY KEY REFERENCES capture.peers(id),
    operation_id text NOT NULL REFERENCES capture.operations(id),
    expires_at timestamptz NOT NULL
);
CREATE INDEX leases_expiry_idx ON capture.leases(expires_at);
CREATE TABLE capture.revoked_deployments (
    id text COLLATE "C" PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64)
);
REVOKE ALL ON ALL TABLES IN SCHEMA capture FROM PUBLIC;
