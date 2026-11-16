-- Narrower management credentials and a separate plugin secret.
--
-- plugin_access maps a plugin ID to read, write or admin. Fleet forwards a
-- plugin call only for a granted plugin; the role grants none. delegate
-- allows acting-for headers. Both are for management credentials only.
--
-- plugin_hash is the SHA-256 of an agent credential's plugin token, the only
-- token plugin introspection matches. Agent credentials issued before this
-- migration have none: plugins refuse them until they are reissued.
ALTER TABLE fleet.credentials
    ADD COLUMN plugin_access jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT credentials_plugin_access_check CHECK (jsonb_typeof(plugin_access) = 'object'),
    ADD COLUMN delegate boolean NOT NULL DEFAULT false,
    ADD COLUMN plugin_hash bytea
        CONSTRAINT credentials_plugin_hash_check CHECK (octet_length(plugin_hash) = 32);
ALTER TABLE fleet.credentials ADD CONSTRAINT credentials_management_scope_check
    CHECK (role <> 'agent' OR (plugin_access = '{}'::jsonb AND NOT delegate));
ALTER TABLE fleet.credentials ADD CONSTRAINT credentials_plugin_hash_role_check
    CHECK (plugin_hash IS NULL OR role = 'agent');
CREATE UNIQUE INDEX credentials_plugin_hash_idx ON fleet.credentials (plugin_hash);
