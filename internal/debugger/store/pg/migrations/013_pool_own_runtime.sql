-- A pool's own build in presence.
--
-- A pool worker also runs its own build, the built-in runtime of its
-- release, without a binary. Its row names that runtime identity, so the
-- pool counts as a worker of it.
ALTER TABLE workers
    ADD COLUMN own text NOT NULL DEFAULT '' CHECK (own = '' OR own LIKE 'sha256:%');
