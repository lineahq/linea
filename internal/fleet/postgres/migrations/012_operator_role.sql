-- The operator role manages groups, configurations and rollouts and may call
-- plugin APIs. It cannot issue or revoke credentials or change plugin
-- lifecycle. The inline role check of fleet.credentials has the default name.
ALTER TABLE fleet.credentials DROP CONSTRAINT credentials_role_check;
ALTER TABLE fleet.credentials ADD CONSTRAINT credentials_role_check
    CHECK (role IN ('admin', 'operator', 'reader', 'agent'));
