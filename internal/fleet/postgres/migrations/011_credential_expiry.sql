-- Admin and reader credentials may be valid until revoked: a NULL expiry
-- never expires. Agent credentials keep a required expiry.
ALTER TABLE fleet.credentials ALTER COLUMN expires_at DROP NOT NULL;
ALTER TABLE fleet.credentials ADD CONSTRAINT credentials_agent_expiry
    CHECK (role <> 'agent' OR expires_at IS NOT NULL);
