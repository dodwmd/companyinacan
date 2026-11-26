-- Verified holder identity (specs/051-holder-identity). A holder record is "established" when a
-- second person confirmed it: person_verified_* (the record is this directory person) or the
-- external_attested_* columns of 003. Existing person_id values are NOT backfilled as verified:
-- they were never checked, and are listed for review (GET /v1/identity/review).
ALTER TABLE stakeholders ADD COLUMN person_linked_by TEXT;
ALTER TABLE stakeholders ADD COLUMN person_verified_by TEXT;
ALTER TABLE stakeholders ADD COLUMN person_verified_on TEXT;
ALTER TABLE stakeholders ADD COLUMN identity_by TEXT;

CREATE TRIGGER stakeholders_verify_sod BEFORE UPDATE ON stakeholders
WHEN NEW.person_verified_by IS NOT NULL AND NEW.person_verified_by IS NOT OLD.person_verified_by
  AND (NEW.person_verified_by = OLD.created_by OR NEW.person_verified_by = IFNULL(NEW.person_linked_by, '')
    OR NEW.person_verified_by = IFNULL(NEW.identity_by, ''))
BEGIN SELECT RAISE(ABORT, 'separation of duties: a person link is confirmed by someone else'); END;
