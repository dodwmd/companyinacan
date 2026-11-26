-- specs/047-csv-import: external_id is the record's id in the system it was imported from (an
-- opaque code, never an email address), so a re-run of an import never creates a second record.
-- It is kept when a record is erased, so a re-import cannot bring erased data back.
ALTER TABLE people ADD COLUMN external_id TEXT;
CREATE UNIQUE INDEX people_external_id ON people (external_id) WHERE external_id IS NOT NULL;
