-- specs/047-csv-import: external_id is the party's id in the system it was imported from (an
-- opaque code), so a re-run of an import never creates a second party. It is kept when a party
-- is erased, so a re-import cannot bring erased data back.
ALTER TABLE parties ADD COLUMN external_id TEXT;
CREATE UNIQUE INDEX parties_external_id ON parties (external_id) WHERE external_id IS NOT NULL;
