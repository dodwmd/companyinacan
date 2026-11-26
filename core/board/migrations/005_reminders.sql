-- Filing reminders reach can-core from this release on (specs/060-reminders). Before it, a service
-- installed by can-core had nowhere to send its events: they were marked announced and left in the
-- outbox. Announce the pending filings again (the first sweep summarises many into one event), and
-- retire the events that were never even attempted so that an old backlog is not published at once.
UPDATE filings SET reminders_sent = ''
    WHERE status = 'pending' AND NOT EXISTS (
        SELECT 1 FROM outbox o WHERE o.type = 'entity.filing.due.v1' AND o.delivered_at IS NOT NULL AND instr(o.payload, filings.id) > 0);

UPDATE outbox SET delivered_at = CAST(strftime('%s', 'now') AS INTEGER) WHERE delivered_at IS NULL AND attempts = 0;
