-- A delegating management credential may name the user it acts for. Events
-- record it for attribution only; fleet never authorizes on it.
ALTER TABLE fleet.events ADD COLUMN acting_for jsonb
    CHECK (acting_for IS NULL OR (jsonb_typeof(acting_for) = 'object' AND octet_length(acting_for::text) <= 1024));
