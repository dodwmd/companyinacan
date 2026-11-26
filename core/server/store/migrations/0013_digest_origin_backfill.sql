-- Mail still queued from before 0011 recorded which module caused a notification would go out as the platform's
-- own, with the module's wording and without its switch and quota. A module's own notices have types starting
-- with its id (and only they carry a title or body); attribute those, so the module's mail rules apply.
UPDATE notifications SET origin = substr(type, 1, instr(type, '.') - 1), authored = 1
WHERE origin = '' AND instr(type, '.') > 1 AND type NOT LIKE 'approval.%'
  AND id IN (SELECT notification_id FROM notify_deliveries WHERE channel = 'email' AND status = 'queued'
             AND at <= (SELECT applied_at FROM schema_migrations WHERE version = 11))
  AND (substr(type, 1, instr(type, '.') - 1) IN (SELECT id FROM modules)
       OR json_extract(data_json, '$.title') IS NOT NULL OR json_extract(data_json, '$.body') IS NOT NULL);
