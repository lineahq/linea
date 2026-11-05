-- M4: generic plugin host. Plugins resolve Collector credentials by token hash,
-- and agent credentials carry bounded labels that plugins interpret.
ALTER TABLE fleet.extension_lifecycle RENAME TO plugin_lifecycle;
ALTER TABLE fleet.credentials
    ADD COLUMN labels jsonb NOT NULL DEFAULT '{}'
        CHECK (jsonb_typeof(labels) = 'object' AND length(labels::text) <= 4096);
CREATE UNIQUE INDEX credentials_token_hash_idx ON fleet.credentials (token_hash);
