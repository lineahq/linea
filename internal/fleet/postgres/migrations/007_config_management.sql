-- M6: optional config management. Version bodies are read only by admins and
-- never copied into events; agents report remote config status through the
-- session writer, which writes only on change.
ALTER TABLE fleet.credentials
    ADD COLUMN remote_config boolean NOT NULL DEFAULT false,
    ADD CHECK (NOT remote_config OR role = 'agent');

CREATE TABLE fleet.configs (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    name text COLLATE "C" NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    description text NOT NULL DEFAULT '' CHECK (octet_length(description) <= 1024),
    revision bigint NOT NULL CHECK (revision > 0),
    latest_version integer NOT NULL DEFAULT 0 CHECK (latest_version >= 0),
    created_at timestamptz NOT NULL,
    created_by text NOT NULL
);

CREATE TABLE fleet.config_versions (
    config_id text COLLATE "C" NOT NULL REFERENCES fleet.configs(id) ON DELETE CASCADE,
    version integer NOT NULL CHECK (version > 0),
    sha256 bytea NOT NULL CHECK (octet_length(sha256) = 32),
    body bytea NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 131072),
    message text NOT NULL DEFAULT '' CHECK (octet_length(message) <= 1024),
    created_at timestamptz NOT NULL,
    created_by text NOT NULL,
    PRIMARY KEY (config_id, version)
);

-- An assignment targets a group or an inline selector, never both. Its
-- version and group cannot be deleted while it references them.
CREATE TABLE fleet.config_assignments (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    name text COLLATE "C" NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    config_id text COLLATE "C" NOT NULL,
    version integer NOT NULL,
    layer smallint NOT NULL CHECK (layer BETWEEN 0 AND 99),
    group_id text COLLATE "C" REFERENCES fleet.groups(id),
    selector text CHECK (octet_length(selector) BETWEEN 1 AND 4096),
    credential_labels jsonb NOT NULL DEFAULT '{}'
        CHECK (jsonb_typeof(credential_labels) = 'object' AND octet_length(credential_labels::text) <= 4096),
    revision bigint NOT NULL CHECK (revision > 0),
    created_at timestamptz NOT NULL,
    created_by text NOT NULL,
    FOREIGN KEY (config_id, version) REFERENCES fleet.config_versions(config_id, version),
    CHECK ((group_id IS NULL) <> (selector IS NULL))
);
CREATE INDEX config_assignments_version_idx ON fleet.config_assignments (config_id, version);
CREATE INDEX config_assignments_group_idx ON fleet.config_assignments (group_id) WHERE group_id IS NOT NULL;

-- Hashes are what the agent echoes back, so they are bounded hex rather than
-- exactly 32 bytes. The error is stored only with the health-error opt-in.
ALTER TABLE fleet.agents
    ADD COLUMN remote_config_hash text CHECK (remote_config_hash ~ '^[0-9a-f]{2,128}$'),
    ADD COLUMN remote_config_status text CHECK (remote_config_status IN ('applying', 'applied', 'failed')),
    ADD COLUMN remote_config_error text CHECK (octet_length(remote_config_error) <= 256),
    ADD COLUMN remote_config_at timestamptz,
    ADD COLUMN config_hold_hash text CHECK (config_hold_hash ~ '^[0-9a-f]{2,128}$');
