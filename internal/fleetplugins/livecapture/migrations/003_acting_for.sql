-- API schema 2.2.0: the user a delegating fleet credential acted for, from fleet's
-- X-Fleet-Acting-* headers. Attribution only; owner_id stays the authority.
ALTER TABLE capture.operations ADD COLUMN acting_for jsonb
    CHECK (acting_for IS NULL OR (jsonb_typeof(acting_for) = 'object' AND octet_length(acting_for::text) <= 1024));
