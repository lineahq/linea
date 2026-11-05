-- Debugger shared store, schema 1.
--
-- Byte payloads (revision content, bundles, evidence, evaluation results and
-- sources, capture segments) live in object storage under their SHA-256; the
-- rows here keep the digest ("sha256:<hex>") and size. Opaque metadata the
-- platform owns is kept as bytea so it reads back byte for byte.

-- One row per installation. Writers that check storage or count limits lock it
-- (SELECT ... FOR UPDATE) first, so limits hold across replicas.
CREATE TABLE installation (
    singleton     boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    id            text        NOT NULL CHECK (id ~ '^[0-9a-f]{32}$'),
    -- generation counts revisions: the workspace generation of the interface.
    generation    bigint      NOT NULL DEFAULT 0 CHECK (generation >= 0),
    run_ordinal   bigint      NOT NULL DEFAULT 0,
    suite_ordinal bigint      NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE artifacts (
    id               text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    project_id       text        NOT NULL CHECK (project_id ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    kind             text        NOT NULL CHECK (kind IN ('configuration', 'fixture', 'scenario', 'setup', 'manifest')),
    -- label is bytea: the interface accepts any bytes, not only UTF-8.
    label            bytea       NOT NULL,
    generation       bigint      NOT NULL CHECK (generation > 0),
    head_revision_id text        NOT NULL,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL
);
CREATE INDEX artifacts_project_idx ON artifacts (project_id, id);

CREATE TABLE revisions (
    id                 text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    artifact_id        text        NOT NULL REFERENCES artifacts (id),
    generation         bigint      NOT NULL CHECK (generation > 0),
    parent_revision_id text        NOT NULL DEFAULT '',
    digest             text        NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    bytes              bigint      NOT NULL CHECK (bytes >= 0),
    created_at         timestamptz NOT NULL,
    actor              text        NOT NULL CHECK (actor <> ''),
    origin             bytea,
    UNIQUE (artifact_id, generation)
);
CREATE INDEX revisions_digest_idx ON revisions (digest);

CREATE TABLE runs (
    id              text        PRIMARY KEY CHECK (id ~ '^run-[0-9a-f]{16}$'),
    project_id      text        NOT NULL CHECK (project_id ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    generation      bigint      NOT NULL CHECK (generation > 0),
    ordinal         bigint      NOT NULL UNIQUE,
    terminal        boolean     NOT NULL,
    metadata        bytea       NOT NULL,
    evidence_digest text        NOT NULL DEFAULT '' CHECK (evidence_digest = '' OR evidence_digest ~ '^sha256:[0-9a-f]{64}$'),
    evidence_bytes  bigint      NOT NULL DEFAULT 0 CHECK (evidence_bytes >= 0),
    bundle_digest   text        NOT NULL DEFAULT '' CHECK (bundle_digest = '' OR bundle_digest ~ '^sha256:[0-9a-f]{64}$'),
    bundle_bytes    bigint      NOT NULL DEFAULT 0 CHECK (bundle_bytes >= 0),
    admitted_at     timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX runs_project_idx ON runs (project_id, ordinal);
CREATE INDEX runs_unfinished_idx ON runs (id) WHERE NOT terminal;
CREATE INDEX runs_evidence_digest_idx ON runs (evidence_digest) WHERE evidence_digest <> '';
CREATE INDEX runs_bundle_digest_idx ON runs (bundle_digest) WHERE bundle_digest <> '';

CREATE TABLE evaluations (
    id            text        PRIMARY KEY CHECK (id ~ '^eval-[0-9a-f]{16}$'),
    run_id        text        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    ordinal       bigint      NOT NULL CHECK (ordinal > 0),
    metadata      bytea       NOT NULL,
    result_digest text        NOT NULL CHECK (result_digest ~ '^sha256:[0-9a-f]{64}$'),
    result_bytes  bigint      NOT NULL CHECK (result_bytes > 0),
    source_digest text        NOT NULL CHECK (source_digest ~ '^sha256:[0-9a-f]{64}$'),
    source_bytes  bigint      NOT NULL CHECK (source_bytes > 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, ordinal)
);
CREATE INDEX evaluations_result_digest_idx ON evaluations (result_digest);
CREATE INDEX evaluations_source_digest_idx ON evaluations (source_digest);

CREATE TABLE suites (
    id         text        PRIMARY KEY CHECK (id ~ '^suite-[0-9a-f]{16}$'),
    project_id text        NOT NULL CHECK (project_id ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    ordinal    bigint      NOT NULL UNIQUE,
    metadata   bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A suite's member runs, extracted from its metadata so that deleting a run
-- can be refused while a live suite lists it.
CREATE TABLE suite_members (
    suite_id text NOT NULL REFERENCES suites (id) ON DELETE CASCADE,
    run_id   text NOT NULL REFERENCES runs (id),
    PRIMARY KEY (suite_id, run_id)
);
CREATE INDEX suite_members_run_idx ON suite_members (run_id);

-- Deleted runs and suites; a deleted ID is never reused.
CREATE TABLE tombstones (
    kind       text        NOT NULL CHECK (kind IN ('run', 'suite')),
    id         text        NOT NULL,
    deleted_at timestamptz NOT NULL,
    PRIMARY KEY (kind, id)
);

CREATE TABLE captures (
    id          text        PRIMARY KEY CHECK (id LIKE 'capture-%' AND length(id) <= 128),
    project_id  text        NOT NULL CHECK (project_id ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    generation  bigint      NOT NULL CHECK (generation > 0),
    closed      boolean     NOT NULL,
    memory_only boolean     NOT NULL,
    metadata    bytea       NOT NULL,
    -- bytes is the decoded size of every segment so far.
    bytes       bigint      NOT NULL CHECK (bytes >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX captures_open_idx ON captures (id) WHERE NOT closed AND NOT memory_only;

CREATE TABLE capture_segments (
    capture_id         text   NOT NULL REFERENCES captures (id) ON DELETE CASCADE,
    seq                int    NOT NULL CHECK (seq >= 0),
    digest             text   NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    bytes              bigint NOT NULL CHECK (bytes > 0),
    encoding           text   NOT NULL DEFAULT '' CHECK (encoding IN ('', 'gzip')),
    uncompressed_bytes bigint NOT NULL DEFAULT 0 CHECK (uncompressed_bytes >= 0),
    PRIMARY KEY (capture_id, seq)
);
CREATE INDEX capture_segments_digest_idx ON capture_segments (digest);

-- Every stored object any row references, with its stored size.
CREATE VIEW object_refs (digest, size) AS
    SELECT digest, bytes FROM revisions
    UNION ALL SELECT evidence_digest, evidence_bytes FROM runs WHERE evidence_digest <> ''
    UNION ALL SELECT bundle_digest, bundle_bytes FROM runs WHERE bundle_digest <> ''
    UNION ALL SELECT result_digest, result_bytes FROM evaluations
    UNION ALL SELECT source_digest, source_bytes FROM evaluations
    UNION ALL SELECT digest, bytes FROM capture_segments;

-- The audit log. seq is gapless: a writer holds the head row's lock until it
-- commits, so sequence order is commit order. at_ns is the event time in Unix
-- nanoseconds.
CREATE TABLE audit_head (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    seq       bigint  NOT NULL DEFAULT 0
);
INSERT INTO audit_head (singleton, seq) VALUES (true, 0);

CREATE TABLE audit_events (
    seq            bigint PRIMARY KEY CHECK (seq > 0),
    at_ns          bigint NOT NULL,
    principal      text   NOT NULL,
    principal_kind text   NOT NULL,
    client         text   NOT NULL,
    request_id     text   NOT NULL DEFAULT '',
    operation      text   NOT NULL,
    permission     text   NOT NULL DEFAULT '',
    scope          text   NOT NULL DEFAULT '',
    resources      text[] NOT NULL DEFAULT '{}',
    outcome        text   NOT NULL CHECK (outcome IN ('allowed', 'denied')),
    code           text   NOT NULL DEFAULT ''
);
CREATE INDEX audit_events_principal_idx ON audit_events (principal, seq DESC);
CREATE INDEX audit_events_at_idx ON audit_events (at_ns);

CREATE TABLE tokens (
    seq        bigint      GENERATED ALWAYS AS IDENTITY UNIQUE,
    id         text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    hash       text        NOT NULL UNIQUE CHECK (hash ~ '^[0-9a-f]{64}$'),
    principal  text        NOT NULL,
    kind       text        NOT NULL CHECK (kind IN ('user', 'service')),
    name       text        NOT NULL DEFAULT '',
    groups     text[]      NOT NULL DEFAULT '{}',
    label      text        NOT NULL,
    created_by text        NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX tokens_principal_idx ON tokens (principal, seq DESC);

CREATE TABLE sessions (
    hash       text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    principal  text        NOT NULL,
    name       text        NOT NULL DEFAULT '',
    groups     text[]      NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
