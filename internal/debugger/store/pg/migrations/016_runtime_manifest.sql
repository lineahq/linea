-- A runtime's manifest in the catalog.
--
-- A runtime built on the server keeps the OCB manifest it was built from,
-- as sent, so it still names its components after its build is trimmed.
-- Earlier entries have none: their manifest is in the object store under
-- their build's inputs, which SQL cannot read, so the platform reads it
-- from the build while the build is kept.
ALTER TABLE runtimes
    ADD COLUMN manifest bytea NOT NULL DEFAULT ''::bytea CHECK (length(manifest) <= 1048576);
