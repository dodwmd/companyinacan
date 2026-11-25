# Research: Jobs and Recruiting (E16a)

Facts about third-party programmes below are general knowledge of how these boards work and were **not re-verified against live documentation** (this was built offline). The docs tell the operator to check each board's current terms.

**R1 Google for Jobs.** Indexed from schema.org `JobPosting` on the careers pages; no agreement. Properties used: title, description (HTML), datePosted, validThrough, hiringOrganization, jobLocation (or TELECOMMUTE with applicantLocationRequirements), baseSalary when pay is published. Expired jobs should be `noindex` or removed, so closed, paused and expired pages are `noindex` with no JSON-LD.

**R2 Indeed and Glassdoor.** Feeds and posting APIs are restricted to approved partners and ATS vendors; Glassdoor runs through Indeed. The Indeed-format XML uses the documented element names (title, date, referencenumber, url, company, city, state, country, postalcode, description, salary, jobtype, category, remotetype). It is a ready file, not an integration, and accepted values (for example `remotetype`) must be confirmed with Indeed when partner access exists.

**R3 LinkedIn.** Job posting and apply APIs are partner-only. A stub returns `needs_partner_access`.

**R4 Resume handling.** No temp files (read-only root filesystem): multipart is streamed with `MultipartReader`. Type is decided by extension plus magic bytes, never by the declared Content-Type. Scanning is a hook; the default reports `skipped`.

**R5 Sealing.** Same scheme as the people module (XChaCha20-Poly1305, AAD = table:id:column, keyring file with the newest key first). Email is sealed and also HMAC-hashed under a separate key for dedupe.

**R6 Retention defaults.** Rejected 12 months, talent pool 24 months with consent, hired 6 months (the people record takes over), diversity aggregates 3 years, closed jobs 3 years. Starting points to review, not advice.

**R7 Lessons applied from the people and work QA.** Scope predicate parenthesised and applied to the whole query; by-id lookups use the same predicate (404, not 403); failed authentications throttled per address, valid credentials never; a request deadline bounds database work (503 on timeout); every read of sensitive data audited before returning and failing closed; no personal data in events; masks identical in every representation; WAL truncate after erasure and retention; unknown JSON fields rejected; LIKE wildcards escaped; the public form never rewrites an existing candidate; scoped admin tokens have no global powers.

**R8 Footprint (measured locally, static linux/amd64 build, 15 MB binary).** Idle RSS 16.0 MiB after start. After 100 job creates and 200 rounds of careers-page plus authenticated list requests: 23.0 MiB RSS (HWM 23.0 MiB). Measured from `/proc/<pid>/status` on the dev host, not in a container. Declared target 30 MB, limit 64 MB.

**R9 Event naming.** The SPEC catalogue lists `job.published.v1`, `application.received.v1`, `offer.accepted.v1`. This slice publishes `job.opened.v1`, `application.received.v1`, `application.stage_changed.v1` and `candidate.hired.v1`. `job.published.v1` and `offer.accepted.v1` stay in the catalogue; offers belong to the `documents` work, and `candidate.hired.v1` is the hand-off until then.
