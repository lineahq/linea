-- Plugin state shared by every core replica. The row is the truth: one epoch
-- per plugin, the plugin process it was issued to, and the database time of
-- the last successful manifest probe by any replica. Readiness is
-- desired = 'enabled' AND instance_id IS NOT NULL AND now() - seen_at < 3 s.
--
-- A row is created by the first successful probe or the first lifecycle
-- change. An absent row means desired = 'enabled' at revision 0, so a row
-- created by a probe starts there, and revision 0 becomes valid.
ALTER TABLE fleet.plugin_lifecycle
    ADD COLUMN epoch text NOT NULL DEFAULT replace(gen_random_uuid()::text, '-', '')
        CONSTRAINT plugin_lifecycle_epoch_check CHECK (epoch ~ '^[0-9a-f]{32}$'),
    ADD COLUMN instance_id text
        CONSTRAINT plugin_lifecycle_instance_id_check CHECK (instance_id ~ '^[0-9a-f]{32}$'),
    ADD COLUMN seen_at timestamptz;
-- Existing rows got their own epoch from the default; the server always
-- supplies one from now on.
ALTER TABLE fleet.plugin_lifecycle ALTER COLUMN epoch DROP DEFAULT;
-- The inline check of migration 003 kept its original table name.
ALTER TABLE fleet.plugin_lifecycle DROP CONSTRAINT extension_lifecycle_revision_check;
ALTER TABLE fleet.plugin_lifecycle ADD CONSTRAINT plugin_lifecycle_revision_check CHECK (revision >= 0);
-- A plugin that was seen has an instance.
ALTER TABLE fleet.plugin_lifecycle ADD CONSTRAINT plugin_lifecycle_seen_check
    CHECK (seen_at IS NULL OR instance_id IS NOT NULL);
