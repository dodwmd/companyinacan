-- E03c: the hand-off from jobs (candidate.hired.v1) starts a person in onboarding. hire_ref is the
-- jobs candidate id (an opaque id, no personal data): it makes the hand-off idempotent, so a
-- redelivered event never creates a second person or a second checklist.
ALTER TABLE people ADD COLUMN hire_ref TEXT;
CREATE UNIQUE INDEX people_hire_ref ON people (hire_ref) WHERE hire_ref IS NOT NULL;
