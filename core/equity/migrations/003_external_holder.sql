-- A grant may leave draft only for a holder linked to a directory person (person_id) or one an
-- independent second person attested as an external holder (not a director or approver). The
-- attester must differ from whoever created the stakeholder; the database repeats that rule.
ALTER TABLE stakeholders ADD COLUMN external_attested_by TEXT;
ALTER TABLE stakeholders ADD COLUMN external_attested_on TEXT;

CREATE TRIGGER stakeholders_attest_sod BEFORE UPDATE ON stakeholders
WHEN NEW.external_attested_by IS NOT NULL AND NEW.external_attested_by IS NOT OLD.external_attested_by AND NEW.external_attested_by = OLD.created_by
BEGIN SELECT RAISE(ABORT, 'separation of duties: the attester must not be the person who created the stakeholder'); END;
