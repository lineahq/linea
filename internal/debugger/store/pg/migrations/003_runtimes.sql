-- The runtime catalog: a name for the worker build whose
-- hello reports identity. hello is the registering worker's hello as the
-- platform encoded it. confirmed_at is NULL until a connected worker of
-- identity said hello.
CREATE TABLE runtimes (
    name          text        PRIMARY KEY CHECK (length(name) BETWEEN 1 AND 128),
    identity      text        NOT NULL CHECK (identity ~ '^sha256:[0-9a-f]{64}$'),
    image         text        NOT NULL DEFAULT '',
    digest        text        NOT NULL DEFAULT '',
    hello         bytea       NOT NULL,
    registered_by text        NOT NULL DEFAULT '',
    registered_at timestamptz NOT NULL,
    confirmed_at  timestamptz
);
CREATE INDEX runtimes_identity_idx ON runtimes (identity);
