-- A pool worker's suspicion of a runtime binary: set on every
-- entry naming the binary, cleared when the entry is saved again.
ALTER TABLE runtimes
    ADD COLUMN suspect_cause text NOT NULL DEFAULT '' CHECK (octet_length(suspect_cause) <= 512),
    ADD COLUMN suspect_pool text NOT NULL DEFAULT '',
    ADD COLUMN suspect_at timestamptz;
