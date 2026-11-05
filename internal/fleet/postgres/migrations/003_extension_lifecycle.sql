CREATE TABLE fleet.extension_lifecycle (
    id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[a-z0-9-]{1,64}$'),
    desired text NOT NULL CHECK (desired IN ('enabled', 'draining', 'disabled')),
    revision bigint NOT NULL CHECK (revision > 0)
);
REVOKE ALL ON fleet.extension_lifecycle FROM PUBLIC;
