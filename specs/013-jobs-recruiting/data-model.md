# Data Model: Jobs and Recruiting (E16a)

SQLite, `core/jobs/migrations`. Timestamps are unix seconds UTC. Columns ending `_sealed` hold `v1.<key-id>.<base64url>` ciphertext bound to `table:id:column`.

| Table | Purpose | Notes |
|---|---|---|
| `stages` | configurable pipeline | kinds initial, active, offer, hired; first initial, last hired |
| `jobs` | requisition and posting in one row | status draft/open/paused/closed, approval none/pending/approved/rejected, `version` for ETag, sections as JSON, optional salary with `publish_salary` |
| `syndication` | last state per job and board | `needs_partner_access`, `feed`, `published`, `closed`, `error` |
| `candidates` | one person, unique by `email_hash` | sealed email, phone, address, salary expectation, notes; consent version and time; talent-pool flag; `retain_until` (NULL while an application is active); `legal_hold`; `erased_at` |
| `applications` | candidate x job, unique pair | stage, status active/rejected/withdrawn/hired, sealed cover letter and rejection note, resume metadata only (opaque key, name, type, size, sha256, scan verdict) |
| `stage_history` | every stage change | actor and time, no free text |
| `interviewers` | assignment of a login to an application | gates scorecards and interviewer visibility; soft-removed (`removed_at`) so "ever assigned" survives |
| `scorecards` | one per (application, author) | rating 1-4, recommendation, sealed comments, draft or submitted |
| `eeo_counts` | voluntary diversity answers as counters | key (month, field, value), `WITHOUT ROWID`: **no per-response row, no id, no job, no timestamp finer than a month**; only whole-organisation aggregates are read |
| `api_tokens` | hashed secrets | role, job scope, expiry, revocation |
| `audit_log` | append-only, hash-chained | triggers refuse UPDATE and DELETE; ids and field names only |
| `erasures` | tombstones (subject, class, date) | unique per subject and class; feed for the backup ledger |
| `outbox` | webhook events | at-least-once, backoff, dead-letter and replay |

## Data classes

| Class | Holds | Default retention |
|---|---|---|
| `candidate-pii` | name, contact, notes, cover letters | 12 months after the last decision (24 with talent-pool consent, 6 after a hire) |
| `candidate-resumes` | resume files and metadata | with `candidate-pii` |
| `candidate-evaluations` | scorecards, rejection notes | with `candidate-pii` |
| `eeo-aggregate` | anonymous diversity rows | 3 years |

Erasure and retention (both write tombstones) purge the three candidate classes and leave an id-only shell (candidate id, application ids, stages, dates) so pipeline reports stay consistent. Shells with no remaining application are deleted by the sweep.

## State rules

Job: draft -> open | closed; open -> paused | closed; paused -> open | closed; closed is final. Application: active -> rejected | hired | withdrawn (by erasure); moves only while active; hire only from an offer-kind stage. Scorecard: draft -> submitted (final).
