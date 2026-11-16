-- Plugin 0.5.0: unused peers are deleted by the running plugin.
-- seen_at is the last time a Collector held a control stream for the peer. The
-- plugin refreshes it for connected peers and deletes peers that no lease and
-- no retained operation refers to once it is older than the retention period.
-- generation_floor is the highest generation ever deleted: a peer created after
-- a delete starts above it, so no generation is ever issued twice.
ALTER TABLE capture.peers ADD COLUMN seen_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX peers_seen_idx ON capture.peers(seen_at);
ALTER TABLE capture.metadata ADD COLUMN generation_floor bigint NOT NULL DEFAULT 0 CHECK (generation_floor >= 0);
