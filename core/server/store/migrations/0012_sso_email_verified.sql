-- Whether the identity provider said the stored address was verified, at the sign-in that stored it: only such
-- an address takes notification mail. Existing rows are unknown, so unverified until the person signs in again.
ALTER TABLE sso_identities ADD COLUMN email_verified INTEGER NOT NULL DEFAULT 0;
