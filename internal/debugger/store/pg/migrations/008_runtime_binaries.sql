-- Runtimes built on the server.
--
-- A runtime built on the server names the binary it runs (binary_*, in object
-- storage under its SHA-256 like every payload) and the build that made it.
-- The build may be trimmed later, so the catalog entry references the binary
-- itself. A runtime registered from an image has all of these empty.
ALTER TABLE runtimes
    ADD COLUMN binary_digest text   NOT NULL DEFAULT '' CHECK (binary_digest = '' OR binary_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN binary_bytes  bigint NOT NULL DEFAULT 0  CHECK (binary_bytes BETWEEN 0 AND 536870912),
    ADD COLUMN binary_os     text   NOT NULL DEFAULT '' CHECK (binary_os = '' OR binary_os ~ '^[a-z0-9]{1,32}$'),
    ADD COLUMN binary_arch   text   NOT NULL DEFAULT '' CHECK (binary_arch = '' OR binary_arch ~ '^[a-z0-9]{1,32}$'),
    ADD COLUMN build_id      text   NOT NULL DEFAULT '' CHECK (length(build_id) <= 128 AND build_id !~ '[[:cntrl:]]'),
    ADD CONSTRAINT runtimes_binary CHECK (
        (binary_digest <> '') = (binary_bytes > 0)
        AND (binary_digest <> '') = (build_id <> '')
        AND (binary_digest <> '') = (binary_os <> '')
        AND (binary_digest <> '') = (binary_arch <> ''));
CREATE INDEX runtimes_binary_digest_idx ON runtimes (binary_digest) WHERE binary_digest <> '';

-- A builder uploads its binary before it finishes the build. The store
-- records the verified upload on the running build, so the object is
-- referenced from the moment it is stored until the build ends, whether
-- FinishBuild then names it or the lease is lost. FinishBuild and the expiry
-- clear it in the statement that leaves 'running'.
ALTER TABLE builds
    ADD COLUMN staged_binary_digest text   NOT NULL DEFAULT '' CHECK (staged_binary_digest = '' OR staged_binary_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN staged_binary_bytes  bigint NOT NULL DEFAULT 0  CHECK (staged_binary_bytes BETWEEN 0 AND 536870912),
    ADD CONSTRAINT builds_staged CHECK (
        (staged_binary_digest <> '') = (staged_binary_bytes > 0)
        AND (staged_binary_digest = '' OR state = 'running'));
CREATE INDEX builds_staged_digest_idx ON builds (staged_binary_digest) WHERE staged_binary_digest <> '';

-- Every stored object any row references now also includes the binaries of
-- catalogued runtimes and the stored binary of a running build.
CREATE OR REPLACE VIEW object_refs (digest, size) AS
    SELECT digest, bytes FROM revisions
    UNION ALL SELECT evidence_digest, evidence_bytes FROM runs WHERE evidence_digest <> ''
    UNION ALL SELECT bundle_digest, bundle_bytes FROM runs WHERE bundle_digest <> ''
    UNION ALL SELECT result_digest, result_bytes FROM evaluations
    UNION ALL SELECT source_digest, source_bytes FROM evaluations
    UNION ALL SELECT digest, bytes FROM capture_segments
    UNION ALL SELECT digest, bytes FROM build_inputs
    UNION ALL SELECT binary_digest, binary_bytes FROM builds WHERE binary_digest <> ''
    UNION ALL SELECT staged_binary_digest, staged_binary_bytes FROM builds WHERE staged_binary_digest <> ''
    UNION ALL SELECT binary_digest, binary_bytes FROM runtimes WHERE binary_digest <> '';
