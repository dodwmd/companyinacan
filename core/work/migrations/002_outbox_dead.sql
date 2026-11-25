-- Events that could not be delivered within the delivery window are kept as "dead" rows so an
-- admin can list and replay them instead of losing them.
ALTER TABLE outbox ADD COLUMN dead_at INTEGER;
CREATE INDEX outbox_dead ON outbox (dead_at) WHERE dead_at IS NOT NULL;
