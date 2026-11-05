-- Runs pinned to a runtime binary.
--
-- A run of a server-built runtime is admitted with the binary it runs on
-- (binary_*, a stored runtime binary under its SHA-256 and the platform
-- "os/arch" it was built for), so pool workers fetch exactly that binary
-- whatever the catalog names later. A run only dedicated workers take has
-- all of these empty.
ALTER TABLE runs
    ADD COLUMN binary_digest   text   NOT NULL DEFAULT '' CHECK (binary_digest = '' OR binary_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN binary_bytes    bigint NOT NULL DEFAULT 0  CHECK (binary_bytes BETWEEN 0 AND 536870912),
    ADD COLUMN binary_platform text   NOT NULL DEFAULT '' CHECK (binary_platform = '' OR binary_platform ~ '^[a-z0-9]{1,32}/[a-z0-9]{1,32}$'),
    ADD CONSTRAINT runs_binary CHECK (
        (binary_digest <> '') = (binary_bytes > 0)
        AND (binary_digest <> '') = (binary_platform <> ''));

-- The pool claim query: queued runs of one runtime pinned to a platform, in
-- priority, then admission, order.
CREATE INDEX runs_binary_queued_idx ON runs (runtime, binary_platform, priority, ordinal)
    WHERE queue_state = 'queued' AND binary_digest <> '';
-- The binary of an unfinished run is a referenced object (object_refs).
CREATE INDEX runs_binary_digest_idx ON runs (binary_digest) WHERE binary_digest <> '' AND NOT terminal;

-- Every stored object any row references now also includes the binary of
-- each unfinished run. A finished run keeps its columns but drops the
-- reference, so the object stays only while a catalog entry, a build or an
-- unfinished run names it. A digest several rows name is one object:
-- consumers count DISTINCT (digest, size).
CREATE OR REPLACE VIEW object_refs (digest, size) AS
    SELECT digest, bytes FROM revisions
    UNION ALL SELECT evidence_digest, evidence_bytes FROM runs WHERE evidence_digest <> ''
    UNION ALL SELECT bundle_digest, bundle_bytes FROM runs WHERE bundle_digest <> ''
    UNION ALL SELECT binary_digest, binary_bytes FROM runs WHERE binary_digest <> '' AND NOT terminal
    UNION ALL SELECT result_digest, result_bytes FROM evaluations
    UNION ALL SELECT source_digest, source_bytes FROM evaluations
    UNION ALL SELECT digest, bytes FROM capture_segments
    UNION ALL SELECT digest, bytes FROM build_inputs
    UNION ALL SELECT binary_digest, binary_bytes FROM builds WHERE binary_digest <> ''
    UNION ALL SELECT staged_binary_digest, staged_binary_bytes FROM builds WHERE staged_binary_digest <> ''
    UNION ALL SELECT binary_digest, binary_bytes FROM runtimes WHERE binary_digest <> '';
