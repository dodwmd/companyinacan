-- Consent checkboxes ticked on a submission (specs/069-lead-adapters): the JSON list of the names
-- of the fields that were of type consent when the lead was stored and were ticked. A text field of
-- the same name is not consent.
ALTER TABLE leads ADD COLUMN consents TEXT NOT NULL DEFAULT '[]';
