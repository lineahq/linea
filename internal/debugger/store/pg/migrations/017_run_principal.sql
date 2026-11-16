-- The principal that submitted each run.
--
-- limits.runs_per_principal counts a principal's unfinished runs at
-- admission, under the installation lock, so every instance sees the runs
-- the others admitted. Runs admitted before this migration have none and
-- count toward no principal.
ALTER TABLE runs
    ADD COLUMN principal text NOT NULL DEFAULT '' CHECK (octet_length(principal) <= 256);

CREATE INDEX runs_principal_unfinished_idx ON runs (principal) WHERE NOT terminal AND principal <> '';
