-- Runtime builds.
--
-- A build is a durable queue entry a builder claims under a lease, as a
-- worker claims a run (002). Its inputs, the manifest and the module source
-- archives, live in object storage under their SHA-256 like every payload;
-- a done build's binary does too. ordinal numbers builds in admission order,
-- which is commit order because admission holds the installation lock, so
-- keyset paging by ordinal never skips a build.

CREATE TABLE builds (
    ordinal          bigint      GENERATED ALWAYS AS IDENTITY UNIQUE,
    id               text        PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 128),
    principal        text        NOT NULL CHECK (length(principal) BETWEEN 1 AND 256),
    name             text        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    -- build_key identifies the inputs: builds of one key produce one binary.
    build_key        text        NOT NULL CHECK (build_key ~ '^sha256:[0-9a-f]{64}$'),
    spec             bytea       NOT NULL CHECK (octet_length(spec) BETWEEN 1 AND 65536),
    state            text        NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    reused_from      text        NOT NULL DEFAULT '',
    builder          text        NOT NULL DEFAULT '',
    attempt          integer     NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    lease_expires_at timestamptz,
    cancel_requested boolean     NOT NULL DEFAULT false,
    -- log_lines counts every line the build logged; build_log keeps the last
    -- 2000 (artifact.MaxBuildLogLines).
    log_lines        integer     NOT NULL DEFAULT 0 CHECK (log_lines >= 0),
    result           bytea       CHECK (octet_length(result) <= 1048576),
    binary_digest    text        NOT NULL DEFAULT '' CHECK (binary_digest = '' OR binary_digest ~ '^sha256:[0-9a-f]{64}$'),
    binary_bytes     bigint      NOT NULL DEFAULT 0 CHECK (binary_bytes >= 0),
    created_at       timestamptz NOT NULL,
    started_at       timestamptz,
    finished_at      timestamptz,
    CONSTRAINT builds_lease CHECK ((state = 'running') = (lease_expires_at IS NOT NULL AND builder <> '')),
    CONSTRAINT builds_finished CHECK ((state IN ('done', 'failed', 'cancelled')) = (finished_at IS NOT NULL)),
    CONSTRAINT builds_binary CHECK ((state = 'done') = (binary_digest <> '') AND (binary_digest = '') = (binary_bytes = 0)),
    CONSTRAINT builds_reused CHECK (reused_from = '' OR state = 'done'),
    CONSTRAINT builds_cancel CHECK (NOT cancel_requested OR state <> 'queued')
);

-- The claim query: queued builds, oldest first.
CREATE INDEX builds_queued_idx ON builds (ordinal) WHERE state = 'queued';
-- Lease renewal by builder, and the expiry sweep.
CREATE INDEX builds_leased_idx ON builds (builder) WHERE state = 'running';
CREATE INDEX builds_lease_expiry_idx ON builds (lease_expires_at) WHERE state = 'running';
-- Admission limits count a principal's unfinished builds.
CREATE INDEX builds_unfinished_idx ON builds (principal) WHERE state IN ('queued', 'running');
-- Reuse finds the newest done build of a key.
CREATE INDEX builds_done_key_idx ON builds (build_key, ordinal DESC) WHERE state = 'done';
-- Lists, newest first, of one principal; builds.ordinal serves everyone's.
CREATE INDEX builds_principal_idx ON builds (principal, ordinal DESC);
-- Trimming keeps the newest terminal builds.
CREATE INDEX builds_terminal_idx ON builds (ordinal DESC) WHERE state IN ('done', 'failed', 'cancelled');
CREATE INDEX builds_binary_digest_idx ON builds (binary_digest) WHERE binary_digest <> '';

-- A build's inputs in order: the manifest (module '') first, then the
-- sources by module.
CREATE TABLE build_inputs (
    build_id text   NOT NULL REFERENCES builds (id) ON DELETE CASCADE,
    ord      int    NOT NULL CHECK (ord >= 0),
    module   text   NOT NULL,
    digest   text   NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    bytes    bigint NOT NULL CHECK (bytes > 0),
    PRIMARY KEY (build_id, ord)
);
CREATE INDEX build_inputs_digest_idx ON build_inputs (digest);

-- The last lines of a build's log, numbered from its first line.
CREATE TABLE build_log (
    build_id text    NOT NULL REFERENCES builds (id) ON DELETE CASCADE,
    line     integer NOT NULL CHECK (line >= 0),
    text     text    NOT NULL,
    PRIMARY KEY (build_id, line)
);

-- Every stored object any row references now includes build inputs and the
-- binaries of done builds (a reused build names the binary it took).
CREATE OR REPLACE VIEW object_refs (digest, size) AS
    SELECT digest, bytes FROM revisions
    UNION ALL SELECT evidence_digest, evidence_bytes FROM runs WHERE evidence_digest <> ''
    UNION ALL SELECT bundle_digest, bundle_bytes FROM runs WHERE bundle_digest <> ''
    UNION ALL SELECT result_digest, result_bytes FROM evaluations
    UNION ALL SELECT source_digest, source_bytes FROM evaluations
    UNION ALL SELECT digest, bytes FROM capture_segments
    UNION ALL SELECT digest, bytes FROM build_inputs
    UNION ALL SELECT binary_digest, binary_bytes FROM builds WHERE binary_digest <> '';

-- Build changes join the change feed.
ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check
    CHECK (kind IN ('run', 'events', 'evaluation', 'suite', 'deletion', 'job', 'build'));

-- Admission, a claim, a cancellation request, log lines and the end each
-- report the build. A lease renewal changes only lease_expires_at and
-- reports nothing.
CREATE TRIGGER builds_insert_change AFTER INSERT ON builds
    FOR EACH ROW EXECUTE FUNCTION record_change('build');
CREATE TRIGGER builds_update_change AFTER UPDATE ON builds
    FOR EACH ROW WHEN (NEW.state IS DISTINCT FROM OLD.state OR NEW.cancel_requested IS DISTINCT FROM OLD.cancel_requested
        OR NEW.log_lines IS DISTINCT FROM OLD.log_lines)
    EXECUTE FUNCTION record_change('build');
