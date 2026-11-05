-- 0.4.0: Collectors authenticate with fleet agent credentials introspected
-- through the host, so plugin-owned deployment revocations are obsolete.
DROP TABLE capture.revoked_deployments;
