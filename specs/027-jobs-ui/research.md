# Research and decisions: Recruiting web UI (E16b)

## Where the UI lives

Inside package `jobs`, next to the Store, mounted by `NewApp` on the existing listener (the
careers pages already share it). Reserved prefixes (`/v1/`, `/careers`, `/feeds/`, `/robots.txt`,
`/healthz`) go to the API and public pages; everything else is the UI, so unknown `/v1/...` paths
keep the API's behaviour and an unknown UI path gets the UI's 404 page.

## Copied patterns, and a shared package that would help

Session store, login (double-submit cookie), CSRF and `Sec-Fetch-Site`, headers, catalogue, golden,
a11y and contrast tests were copied from the merged people UI (`core/people/ui*.go`). The work UI
(PR #35) was not read or reused. After three UIs the copies are near-identical (about 300 lines of
session and middleware code plus the shared test helpers). **Proposal, not done here:** a
`core/uikit` package holding `sessionStore`, cookie naming, the headers function, `constEq`, the
request-id error rendering and the a11y/contrast checkers, parameterised by an `Authenticator` and a
principal type. Each module keeps its pages, messages and routes. It was left out of this PR on
purpose (no refactor of other modules) and because the three `Principal` types differ (roles and
permissions in people, one role plus a job list here), so a generic parameter or a small interface
is a design decision for its own epic.

## Authentication and who is "the person"

The jobs module has no person table: a token carries one role and an optional job list, and that is
the identity. So login needs only a token that authenticates; the subject is shown in the header.
Hiring managers match jobs by the `hiring_manager` login, which is the token subject.

## Authorisation: what the UI may ask

The Store methods already check the role and apply the scope clause. For presentation the UI needs a
few answers (may I see the requisition form, is this application mine to score, which links to draw).
These are computed in `svc_web.go` from the same `Can` matrix: `Caps` (booleans), `Require` (the API
guard's check, with the denial audit), `IsAssigned`. A source test keeps every `Can`, role compare,
SQL and predicate out of `ui*.go`.

## Stage-move reasons

The service recorded who moved an application but not why. The lesson "moves need a bounded reason"
would otherwise be theatre, so migration 004 adds `stage_history.note_sealed`, sealed with the same
keyring (additional data: table, sequence number, column), added to key rotation and purged with the
evaluations class on erasure or retention. It is readable by the roles that read rejection notes
(admin, recruiter). `MoveApplication` (the API) is unchanged; the UI uses `MoveApplicationNote`,
which makes the note required, 1 to 500 characters.

## Blind feedback in the page

`ListScorecards` returns colleagues' cards only to callers who may see them and a flag
`others_hidden` otherwise. The page does not render the flag. It shows one constant notice to
everyone who is assigned and has not submitted (plus anyone the flag marks, such as a removed
interviewer), so the notice's presence says "you are on the panel", never "colleagues have
submitted". An interviewer cannot even reach a page of an application they are not actively assigned
to (404), and a removed interviewer loses the page altogether; a removed hiring manager keeps reading
but stays blind.

## Diversity report

`EEOAggregate` returns the answer names of a question with counts hidden when a cell is rare. The
page drops the names too: the existence of a "prefer to self-describe" answer in a month with one
respondent is information. It is closed months only, as the service says, so it is empty until the
first month ends.

## Audit granularity of reads

`GetApplication` audits the read of the sealed fields it unseals (cover letter, rejection note) even
though the detail page does not draw them, because the service unseals them for the caller's role
before the UI chooses what to show. The page never renders them; the reveal re-reads them and is
audited again. A finer service split (a metadata read that does not unseal) would make the audit
exact; recorded as a follow-up rather than changing the API's behaviour in this PR.

## Talent-pool search

`SearchTalentPool` adds a bounded name-prefix filter to the existing talent-pool query; there is no
email search in the UI. The API's email lookup (duplicate detection) stays an API feature.

## Resume download

The API sets the stored (sniffed) content type; the UI always serves `application/octet-stream` as an
attachment with a sanitised name. A browser never renders an applicant's file.

## Throttling and deadlines

Per-subject request budget 600/min (the API's default); failed logins 10/min per address; reveals 10/min per
subject. Every request gets `db_timeout_seconds`; the rate limiter is the module's fixed-window one.

## What was not verified

No browser, screen reader or phone test; no cluster. The compose smoke addition (`dev/compose/smoke.sh`)
was written and shellchecked but the stack was not rebuilt or run for this change.
