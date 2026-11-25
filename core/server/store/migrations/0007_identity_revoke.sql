-- E13d: identity revocation for offboarded people (specs/028-identity-provision).

-- People that the people module (the authenticated source of person.offboarded.v1) reported as
-- offboarded. party_id is the directory id; subject_ref is the first 32 hex characters of the
-- SHA-256 of the person's sso_subject, so the account link can be checked without the subject or
-- any other personal data ever being in the event. Only ids, a hash prefix and a date are kept.
CREATE TABLE offboarded_parties (
  party_id       TEXT PRIMARY KEY,
  subject_ref    TEXT NOT NULL DEFAULT '',
  effective_date TEXT NOT NULL,
  recorded_at    INTEGER NOT NULL
);
