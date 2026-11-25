# Research and decisions: E17a equity

> Tool, not legal, tax or securities advice. Nothing here is a statement of what any jurisdiction requires.

## Decisions

**Quantities as integers of 1/10000 share.** Whole shares are the norm, but fractional issuance exists and
floats must never touch share counts. An int64 with a cap of 100 billion shares keeps products with 10000
basis points and 600 months inside range; `math/big` is used where products could exceed it (vesting share,
cost, ownership, round model). JSON carries decimal strings; a JSON number is parsed textually.

**Cumulative rounding.** Rounding each instalment drifts from the total. The calculator rounds the
cumulative amount (`floor`, `nearest`, `fractional`) and returns the exact total at the end; as the
cumulative function is monotone, so is the result.

**Calendar rule.** Adding whole months to the commencement date and clamping to the last day of the month
gives the common behaviour (31 Jan start vests 28 Feb, 29 Feb start vests 28 Feb in common years) and is
simple to state and test. Plans that count differently need a different rule; the schedule is data, the
calculator is replaceable.

**Termination cut-off inclusive.** A vesting date equal to the termination date counts. This is a choice,
documented; it is not derived from any plan text.

**Acceleration as data.** Clauses (`single`/`double`, triggers, percent of unvested, survives termination)
and recorded trigger events feed the calculator. The service never decides that something is a change of
control; a person records it and it is audited.

**Immutability in two layers.** Grant terms frozen after submission in the service and by trigger; amending
creates a new version that supersedes the old on approval, so the pool is never double counted after
approval and there is always a complete history.

**Board approval in-service, engine as a seam.** can-core's engine (E11a) does not yet drive module
approvals. The `ApprovalEngine` interface lets it, and a fake proves the contract (decider bound by SoD),
without blocking the slice. Default: the `board` role approves; `admin` deliberately cannot.

**Pool accounting.** Granted outstanding counts from approval (so the pool is reserved before activation),
releases on cancel, forfeit and expiry, and counts exercised shares as consumed. The approval transaction
re-checks it; one connection serialises it.

**Cap table as of a date.** Derived from dated ledger rows plus grant dates; a mutable balance table would
make past dates unanswerable. Cost: queries scan an entity's rows (bounded, capped at 100000 grants and 20000
holders).

**Ownership in ppm by largest remainder.** Percentages that sum to 100.0000 matter in a legal document; the
method gives exact sums and is property tested.

**Round modelling.** SPEC.md asks for pre/post-money SAFEs and notes as data with conversion computed by a
simple documented formula. The formula (see `docs/equity.md`) is intentionally simple and labelled
illustrative; it is not what a term sheet or SAFE says.

**Erasure vs legal retention.** The ledger refers to stakeholders by opaque id, so pseudonymising the sealed
identity satisfies "keep the ledger, erase the person" without crypto-shredding. Per-subject keys would allow
shredding but were not built (a gap). Erasure of a holder with live grants is refused rather than silently
cancelling them.

**Self-service identity link.** The token carries the directory person id (from the introspection answer);
the stakeholder row stores the same id. The equity service holds no directory data and does not know whether
a person left; it relies on the token.

## What is unverified

No cluster, no container build, no load test. The introspection answer's `person_id` field is a contract with
can-core that is not yet implemented there. Approval-engine adapter, Helm templates, admin UI and portals are
absent. Equity law and tax rules were deliberately not modelled; any claim that a figure is "correct" for a
jurisdiction is out of scope.
