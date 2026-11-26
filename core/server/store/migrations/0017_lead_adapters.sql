-- Lead adapters (specs/067-lead-adapters). `via` marks a subscription served by an adapter inside
-- can-core. lead_links records what an adapter made from a lead, so that erasing the lead can undo
-- it. Ids only: no name, address or message is ever stored here.
ALTER TABLE subscriptions ADD COLUMN via TEXT NOT NULL DEFAULT '';

CREATE TABLE lead_links (
  module_id  TEXT    NOT NULL,
  lead_id    TEXT    NOT NULL,
  -- the other application's own id (subscriber or conversation number); '' until known
  ref        TEXT    NOT NULL DEFAULT '',
  done       INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (module_id, lead_id)
);
CREATE INDEX lead_links_lead ON lead_links(lead_id);
