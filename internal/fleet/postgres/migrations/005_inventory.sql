-- v2 inventory: allowlisted attributes with promoted, indexed columns, health
-- detail, a bounded merged component list and the effective-config hash. Raw
-- configuration is never stored.
ALTER TABLE fleet.agents
    ADD COLUMN attributes jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(attributes) = 'object' AND octet_length(attributes::text) <= 32768),
    ADD COLUMN health_status text CHECK (octet_length(health_status) <= 64),
    ADD COLUMN health_error text CHECK (octet_length(health_error) <= 256),
    ADD COLUMN components jsonb CHECK (jsonb_typeof(components) = 'array' AND octet_length(components::text) <= 65536),
    ADD COLUMN components_unhealthy integer NOT NULL DEFAULT 0 CHECK (components_unhealthy BETWEEN 0 AND 256),
    ADD COLUMN config_hash text CHECK (config_hash ~ '^[0-9a-f]{64}$');
ALTER TABLE fleet.agents
    ADD COLUMN service_name text COLLATE "C" GENERATED ALWAYS AS (COALESCE(attributes->>'service.name', '')) STORED NOT NULL,
    ADD COLUMN service_version text COLLATE "C" GENERATED ALWAYS AS (COALESCE(attributes->>'service.version', '')) STORED NOT NULL,
    ADD COLUMN host_name text COLLATE "C" GENERATED ALWAYS AS (COALESCE(attributes->>'host.name', '')) STORED NOT NULL,
    ADD COLUMN k8s_namespace text COLLATE "C" GENERATED ALWAYS AS (COALESCE(attributes->>'k8s.namespace.name', '')) STORED NOT NULL,
    ADD COLUMN k8s_pod text COLLATE "C" GENERATED ALWAYS AS (COALESCE(attributes->>'k8s.pod.name', '')) STORED NOT NULL;

-- Selectors on other keys use containment (@>) and existence (?).
CREATE INDEX agents_attributes_idx ON fleet.agents USING gin (attributes);
CREATE INDEX agents_service_name_idx ON fleet.agents (service_name, id);
CREATE INDEX agents_service_version_idx ON fleet.agents (service_version, id);
CREATE INDEX agents_host_name_idx ON fleet.agents (host_name, id);
CREATE INDEX agents_k8s_namespace_idx ON fleet.agents (k8s_namespace, id);
CREATE INDEX agents_k8s_pod_idx ON fleet.agents (k8s_pod, id);
-- Retention and capacity eviction remove the longest-disconnected agents.
CREATE INDEX agents_disconnected_idx ON fleet.agents (disconnected_at, id) WHERE owner_replica IS NULL;

-- Selector groups are evaluated at read time; static groups keep members.
ALTER TABLE fleet.groups
    ADD COLUMN kind text NOT NULL DEFAULT 'static' CHECK (kind IN ('static', 'selector')),
    ADD COLUMN selector text CHECK (octet_length(selector) BETWEEN 1 AND 4096),
    ADD CHECK ((kind = 'selector') = (selector IS NOT NULL));
