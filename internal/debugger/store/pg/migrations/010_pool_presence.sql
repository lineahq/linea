-- Pool workers in presence.
--
-- A pool worker is recorded under "pool:<os>/<arch>" with the runtime
-- identities it holds running, so an instance whose pools would start a
-- runtime cold leaves its runs briefly to a warm pool of another instance.
ALTER TABLE workers
    ADD COLUMN warm text[] NOT NULL DEFAULT '{}' CHECK (cardinality(warm) <= 64);
