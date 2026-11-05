-- Deployment tokens enroll multiple Collectors without pre-registering UIDs.
-- Keep existing tokens, expiry and revocation; remove UID binding completely.
ALTER TABLE fleet.credentials DROP COLUMN agent_id;
