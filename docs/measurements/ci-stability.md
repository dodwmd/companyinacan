# CI stability and test speed

Maintenance sweep of the required `unit` job (`make unit`: `go test -race` over `./cmd/... ./core/...
./test/wiring/...`) and of the advisory k3d jobs. Measured on a 32-CPU workstation; the contended runs
pin `go test` to 4 CPUs (`taskset -c 0-3`) and add 4 busy shell loops on the same CPUs, which is
harsher than a 4-vCPU GitHub runner and surfaces timing assumptions.

## Method

- Baseline: `go test -race -count=1 -json ./cmd/... ./core/... ./test/wiring/...`, per-test and
  per-package `Elapsed` from the JSON stream.
- Contended: the same under `taskset -c 0-3` with 4 busy loops, `-count=3 -shuffle=on`.
- `scripts/flaky-hunt.sh` automates the contended loop (`ITER`, `PKGS`, `LOAD`, `RACE`).

## Findings

Across 3 shuffled, contended `-race` runs of the whole tree (about 7 minutes) no test failed; the
wall-clock assumptions below were nevertheless the ones that failed in CI, and several sat within a
small factor of their limit under contention. Tests over 5 s on the unloaded baseline:

| Test | Unloaded | Contended | Action |
| --- | --- | --- | --- |
| core/server/audit `TestVerifyReleasesTheConnectionBetweenChunks` | 17.0 s | n/a | Rewritten: 3000 entries, chunk 100, asserts a concurrent query on the single connection is served while Verify walks (a count, no 100 ms threshold). About 1.7 s. |
| core/work `TestSearchNeverLeavesTokenScope` | 16.6 s | 22-35 s | 2000 seeded random queries instead of 6000 (the exhaustive pool is unchanged); `CAN_TEST_THOROUGH=1` restores 6000. About 5 s. |
| core/server/installer `TestUnguardedDAGUseCannotKillTheParent`, `TestGoroutinesReturnToBaselineAfterChildFailures`, `TestRepeatedReconcilesDoNotStackRenders` | 10-11 s | 13-22 s | Spawn real render children; inherent. Hang guards widened (see below). |
| core/people `TestFieldAccessInvariantsExhaustive`, `TestPolicyInvariantsExhaustive` | 5.8 s | 8-13 s | Exhaustive by design, left as is. |
| core/finance `TestAuthThrottleNeverBlocksValidCredentials` | 7.6 s | 7.6 s | 30 failures each pay the 300 ms production penalty; left (penalty is not injectable without touching production wiring). |
| core/provision `TestWaitingRunsDoNotStarveDueWork`, core/people `TestErasureAuditedEvenWhenScrubFails`, core/forms `TestServeEndToEndAndGracefulShutdown`, core/server/sso `TestClaimsAreEnforced` | 5-6 s | 5-6 s | Left. |

Wall-clock assertions replaced or loosened (all were asserting "does not hang" or "is linear", not a
specific speed):

- core/work `TestFailuresFromAnAddressOverTheLimitAreSlowedNotValidTokens`: "valid token under 150 ms"
  became "the fastest of 5 valid requests is under the 300 ms penalty" (a delayed request always costs
  at least the penalty, so the minimum does not depend on load).
- core/obs `TestHealthCallerWhoGivesUpGetsAnAnswerQuickly`: the check now blocks on a channel, so Check can
  only return by honouring the caller's context; the 150 ms bound is gone. `TestHealthConcurrentCallersShareOneRun`
  lost its 400 ms bound (the run count is the assertion) and its run is longer so all callers join it.
- core/obs 1 s to 10 s, core/equity 2 s to 30 s, core/provision 3 s to 30 s, core/forms 3 s to 30 s
  (backoff is `time.Hour`, so a hang is unmistakable), core/server/installer 10 s and 15 s to 60 s,
  core/work markdown render 500 ms to 10 s per case and 1 s to 20 s for the page (the inputs are hostile
  and quadratic behaviour would take minutes).
- core/server/runtime `TestRunServesTwoSeparateListenersAndShutsDownGracefully`: code shutdown bound 30 s,
  test guard 120 s (a guard against a hang, not a measurement).

Already fixed before this sweep (kept for the record): core/jobs `TestEventsCarryNoPersonalData`
(random ids containing `555`), core/backup `TestLedgerSealOpen`, installer render children under `-race`
(`GORACE=atexit_sleep_ms=0`). `test/wiring` `TestHireHandOffStartsOnboardingInPeople` already uses a fake
clock; it did not fail in any contended run and no further cause was found. If it recurs, run
`PKGS=./test/wiring/... ITER=100 sh scripts/flaky-hunt.sh` and attach the failure to an issue.

## `make unit`

Now `go test -race -count=1 -shuffle=on`. Shuffle passed cleanly in the contended runs (no order
dependencies found), so it is enabled. A failing run prints `-test.shuffle SEED`; reproduce with
`go test -race -shuffle=SEED <pkg>`. `-count=1` makes the result independent of the test cache. `-p` is
left at its default (GOMAXPROCS): the contended runs show the CPU, not package parallelism, is the limit.

## Timings

| Run | Before | After |
| --- | --- | --- |
| `make unit` (32 CPUs, uncached) | 47.7 s | 43.5 s |
| `make test` (lint, unit, contract, licences) | n/a | 50.4 s (limit 2.5 min) |
| 3x contended (4 CPUs + 4 busy loops), `-shuffle=on` | 7 min 13 s, 0 failures | 7 min 16 s, 0 failures (CPU-bound: the saved seconds are inside the noise; one extra package run overlapped it) |

## k3d integration flakes (non-required `footprint` workflow)

- `TestModuleEgressDeniedToNodeAndAPI`, positive control "an unpolicied pod could not reach node":
  `pollReachPort` now first settles the cluster once (nodes Ready, kube-system deployments Available, all
  kube-system pods Running or Completed), then retries with bounded backoff (2 s growing to 15 s, about 4
  minutes in all) instead of 15 fixed attempts. A probe pod that could not run is an error that is retried,
  never counted as BLOCKED (`blockedWithin` no longer treats it as a verdict either).
- `pollReach` (default-deny controls) used 6 immediate attempts with no pause, so it never waited for
  kube-router to sync; it now backs off for up to 2 minutes and uses a fresh pod name per attempt.
- platform-base, "k3d could not start a docker container": `can dev up` (`core/cluster`) now retries
  `k3d cluster create` up to 3 times, deleting the half-created cluster and pausing 5 s then 10 s between
  attempts; `dev/footprint/measure.sh` does the same on its direct `k3d cluster create` path.

Residual flakiness: these jobs remain `continue-on-error`. Runner docker failures that persist across three
attempts, image pulls from Docker Hub (busybox) hitting rate limits, and kube-router sync taking longer than
the budgets above will still fail them. NetworkPolicy not covering pod-to-node traffic on k3s is a known
product finding (issue #13), unrelated to flakiness. None of this could be run here (no k3d cluster); the
changes are covered by unit tests (`core/cluster`) and `go vet -tags integration`.

## Tooling

- `scripts/flaky-hunt.sh`: repeats the suite with shuffle and busy-loop load, prints and writes
  `failing-tests.txt` (package, test, number of failures).
- `.github/workflows/flaky-hunt.yml`: nightly (02:23 UTC) and on dispatch, 20 iterations, uploads the failing
  test names. Not a required check.

## Quarantined tests

None.
