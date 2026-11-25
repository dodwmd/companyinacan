# Feature Specification: Project Foundation (Epic E01)

**Feature Branch**: `001-project-foundation`

**Created**: 2026-11-24

**Status**: Implemented (epic E01 is done)

**Input**: User description: "Epic E01 Foundation: the buildable, tested skeleton everything else plugs into — repo layout, `can` CLI skeleton and thin Makefile, validated `company.yaml` and `module.yaml` schemas with contract tests and fake-data examples, a licence check with generated third-party licence notices, CI, and open-source hygiene files. No Kubernetes, provider, can-core service or module implementation."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Describe a company and a module in validated files (Priority: P1)

A contributor (or the author) writes a company description (`company.yaml`) and a module manifest
(`module.yaml`) and gets immediate, precise feedback on whether they are valid — before anything
is deployed. Invalid files are rejected with an error that names the file, the field and the
reason.

**Why this priority**: The two schemas are the contract every later epic builds against (modules,
providers, can-core, the admin UI). Nothing else can start without them.

**Independent Test**: Run the validator against the shipped valid examples (passes) and against a
set of deliberately broken fixtures (each fails with a specific, field-level message). Delivers a
usable contract even with no other feature present.

**Acceptance Scenarios**:

1. **Given** the example company repo and example module manifest, **When** they are validated,
   **Then** both pass with no errors.
2. **Given** a manifest missing a required field (for example `provider.licence` or
   `resources.ram_mb`), **When** it is validated, **Then** validation fails and the message names
   the file, the field path and what is missing.
3. **Given** a manifest where `ram_limit_mb` is lower than `ram_mb`, or a capability, event or
   permission name is malformed, **When** it is validated, **Then** validation fails with a message
   that identifies the offending field.
4. **Given** a manifest using a field the schema does not know, **When** it is validated, **Then**
   it fails (typos are not silently accepted).
5. **Given** a manifest that declares a sensitive data class, **When** it is validated, **Then**
   it must also declare a retention value for that class.

---

### User Story 2 - Use the `can` command-line tool and `make` targets (Priority: P1)

The author runs `make test` and the project's checks pass. They run the `can` tool to create a
new company description, check their machine for prerequisites, validate a company repo, and see
the tool's version. Commands that belong to later epics are present but clearly say they are not
implemented yet.

**Why this priority**: The CLI and Makefile are the project's front door (`make up`, `make dev`
in the spec); the skeleton fixes the command names, exit codes and output conventions early.

**Independent Test**: Run each command in a clean checkout and verify its output and exit status,
including `can init` in an empty directory producing a company description that validates.

**Acceptance Scenarios**:

1. **Given** an empty directory, **When** the user runs `can init` and answers the prompts (or
   passes name, domain and provider as options), **Then** a `company.yaml` is written that passes
   validation, and nothing outside the target directory is touched.
2. **Given** a directory that already has a `company.yaml`, **When** `can init` is run, **Then** it
   refuses to overwrite it unless explicitly told to.
3. **Given** a machine missing a required tool, **When** the user runs `can doctor`, **Then** it
   lists each prerequisite as found or missing with a one-line remedy, and exits non-zero if a
   required one is missing.
4. **Given** any machine, **When** the user runs `can version`, **Then** the tool's version and
   build information are printed.
5. **Given** the user runs `can apply`, `make dev` or `make up`, **When** the feature is not yet
   built, **Then** the command prints that it is not implemented yet, names the epic that will
   provide it, and exits non-zero (a distinct not-implemented status for `can` commands) without
   side effects.
6. **Given** a company repo, **When** the user runs the validate command, **Then** it checks
   `company.yaml` and every module manifest it references and reports all problems in one run.

---

### User Story 3 - Licence compliance is checked automatically (Priority: P2)

A contributor adds or changes a module that wraps a third-party application. A check reads every
module manifest and fails the build if an upstream licence is missing, or is not an accepted
open-source licence and the module is not explicitly marked as source-available. The check also
produces the `THIRD_PARTY_LICENSES` notice file from the manifests.

**Why this priority**: Open-source hygiene is a constitutional requirement (Principle VI) and is
far cheaper to enforce from the first module than to retrofit.

**Independent Test**: Run the check on fixtures covering: accepted licence, missing licence,
unknown licence, source-available with and without the explicit mark; verify exit status and the
generated file's content.

**Acceptance Scenarios**:

1. **Given** manifests whose licences are all accepted open-source licences, **When** the check
   runs, **Then** it passes and writes a `THIRD_PARTY_LICENSES` file listing each wrapped
   application, its licence and its upstream address.
2. **Given** a manifest with no upstream licence, **When** the check runs, **Then** it fails and
   names the module.
3. **Given** a manifest with a licence not on the accepted list and no source-available mark,
   **When** the check runs, **Then** it fails and names the module and the licence.
4. **Given** a manifest with a non-accepted licence explicitly marked source-available, **When**
   the check runs, **Then** it passes but the generated notice lists the module under a separate
   "source-available (opt-in)" heading.
5. **Given** the committed `THIRD_PARTY_LICENSES` is out of date with the manifests, **When** the
   check runs in CI mode, **Then** it fails and says how to regenerate it.

---

### User Story 4 - Every change is checked automatically and contributors know the rules (Priority: P2)

A contributor opens a pull request. Automated checks run lint, unit tests, schema contract tests,
the licence check and a sign-off (DCO) check, and report clearly which failed. The repository
contains the contributor documentation and templates that explain how to contribute, report a
security issue, and what behaviour is expected.

**Why this priority**: Protects quality from the first line of code and prepares the repository
to be opened to outside contributors, as the spec intends.

**Independent Test**: Open a pull request containing a deliberately failing test, a missing
sign-off and a bad manifest in turn; each is reported by its own named check. Review the hygiene
files for completeness against a short checklist.

**Acceptance Scenarios**:

1. **Given** a pull request whose commits all carry a sign-off line and whose checks pass,
   **When** CI runs, **Then** every required check is green.
2. **Given** a commit without a sign-off line, **When** CI runs, **Then** the sign-off check fails
   and its message explains how to add the line.
3. **Given** a failing unit test, a failing schema contract test, or a failing licence check,
   **When** CI runs, **Then** the corresponding named check fails independently of the others.
4. **Given** a new contributor, **When** they read `CONTRIBUTING`, **Then** it explains the Spec
   Kit workflow, the sign-off rule, how to run the checks locally, and where epics live.
5. **Given** someone who finds a vulnerability, **When** they read `SECURITY.md`, **Then** it tells
   them how to report it privately and what to expect.

---

### Edge Cases

- A manifest file is empty, is not valid YAML, or contains a tab-indentation error: validation
  reports a parse error with line information rather than a schema error.
- A company description lists a module that has no manifest, or two modules provide the same
  exclusive capability without a designated primary: validation reports both problems together.
- Duplicate module ids, or an id that would collide on a subdomain: reported.
- `can init` run without a terminal (CI, scripts): it must not hang; it requires options or fails
  with a clear message.
- `can doctor` on a machine with no network access: still works; it only inspects local tools.
- A wrapped application's licence is expressed with a licence-expression (for example "A OR B"):
  accepted only if every alternative the operator could be bound by is accepted, or at least one
  accepted alternative is explicitly selected in the manifest.
- Running the licence check on a repository with no modules yet: passes and writes an empty notice.
- The committed example files drift from the schema: the contract tests fail.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The repository MUST have the directory layout described in `SPEC.md` §8 (core,
  modules, schemas, deploy, dev, docs, examples, templates), each with a short README stating its
  purpose, so later epics add files in agreed places.
- **FR-002**: The project MUST provide a machine-readable schema for the company description and
  for the module manifest, covering every field `SPEC.md` §4.1 and §3.3 define, including
  permissions, approvals, data classes with retention, published and subscribed events, provided
  and consumed capabilities, resource requests and limits, hooks and upstream licence information.
- **FR-003**: Validation MUST reject unknown fields, missing required fields, malformed names
  (module ids, capability names, event names with version suffix, permission names), invalid
  value ranges (for example a RAM limit below its request) sensitive data classes without a
  retention value, and sensitive data classes declared by a module that wraps a third-party app
  (Constitution V), and MUST report every problem found in one run with file, field path and reason.
- **FR-004**: Schemas MUST carry a schema version; validation MUST reject manifests declaring an
  unsupported schema version with a message naming the supported versions.
- **FR-005**: The project MUST ship at least one valid example company repository and one valid
  example module manifest using only fake data, plus a set of invalid fixtures that each
  demonstrate one validation failure; contract tests MUST assert every fixture's expected outcome.
- **FR-006**: The `can` tool MUST provide `init`, `doctor`, `validate`, `licences`, `apply` and `version`
  commands with consistent help text, exit codes (success, failure, not-implemented) and an option
  for machine-readable output.
- **FR-007**: `can init` MUST create a valid company description from name, domain and provider
  (prompted when interactive, given as options otherwise), MUST NOT overwrite existing files
  without an explicit option, and MUST NOT require network access.
- **FR-008**: `can doctor` MUST check local prerequisites named in `SPEC.md` §3.6 and report each
  as found, missing (with remedy) or optional, without modifying the machine.
- **FR-009**: `can apply`, `make dev` and `make up` MUST exist as stubs that state they are not
  implemented, reference the providing epic, exit non-zero (the `can dev`, `can up` and `can apply` commands use a distinct
  not-implemented status; `make` itself reports a generic failure status) and have no side effects.
- **FR-010**: A single `make test` MUST run the full local check suite (lint, unit tests, contract
  tests, licence check) and `make` MUST list the available targets.
- **FR-011**: The licence check MUST read every module manifest, apply the accepted-licence list
  and source-available rule from `SPEC.md` §5, fail with a message naming the module for any
  violation, and generate `THIRD_PARTY_LICENSES` deterministically (same input, same bytes).
- **FR-012**: The accepted-licence list MUST be a reviewable data file in the repository, not
  hard-coded, so changes to it are visible in review.
- **FR-013**: CI MUST run on every pull request and push to the main branch and MUST include
  separately named checks for lint, unit tests, schema contract tests, licence check and sign-off
  (DCO); a failure in any MUST block merging once branch protection is enabled.
- **FR-014**: The repository MUST contain `CONTRIBUTING` (workflow, sign-off, local checks, Spec
  Kit and epics), `SECURITY.md` (private reporting, response expectations), `CODE_OF_CONDUCT`,
  issue templates (bug, feature/epic) and a pull-request template that includes a
  constitution-compliance checklist.
- **FR-015**: No file added by this feature may contain real credentials, personal data or real
  company data; examples MUST use obviously fake values and reserved example domains.
- **FR-016**: The tool and checks MUST run on Linux with no network access and no
  container runtime required.

### Key Entities

- **Company description**: the operator's declaration of a company: name, domain, provider,
  enabled modules, primary providers for exclusive capabilities, optional backup and email
  settings. Source of truth for what should be running.
- **Module manifest**: a module's self-description: identity, version, upstream provider and
  licence, resources, requirements, provided and consumed capabilities, published and subscribed
  events, permissions, approval types, data classes with retention, hooks.
- **Schema**: the versioned, machine-readable definition that both of the above are validated
  against.
- **Fixture**: a valid or deliberately invalid example file with a recorded expected result,
  used by contract tests.
- **Accepted-licence list**: the reviewable set of licence identifiers allowed for wrapped
  applications.
- **Third-party notice**: the generated `THIRD_PARTY_LICENSES` document.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A contributor with a clean checkout runs `make test` and gets a complete pass/fail
  result in under 2 minutes on an ordinary laptop with no network access.
- **SC-002**: 100% of the invalid fixtures are rejected, each with a message that names the file
  and field, and 100% of valid examples pass (asserted by automated tests).
- **SC-003**: `can init` followed by validation succeeds on the first try in an empty directory
  without the user editing any file, in under 30 seconds.
- **SC-004**: A deliberately introduced licence violation, missing sign-off, failing test or
  invalid manifest is caught by CI and attributed to the correct named check in 100% of trial runs.
- **SC-005**: A new contributor can find how to contribute, run the checks and report a
  vulnerability from the repository's top-level files alone, verified by a review against the
  checklist in FR-014.
- **SC-006**: The generated third-party notice is byte-for-byte identical across two runs on
  different machines for the same inputs.

## Assumptions

- The language, layout and tooling choices already fixed by the constitution and `SPEC.md` §7
  apply; this spec describes outcomes, not implementation.
- Schema field sets follow `SPEC.md` §4.1 and §3.7 as they stand; later epics may add optional
  fields through schema version increments, not by breaking this one.
- The accepted-licence list starts with common OSI-approved licences (for example Apache-2.0, MIT,
  BSD, MPL-2.0, GPL and AGPL families, LGPL) and is extended by pull request.
- Linux is the only supported developer and CI platform for now (there are no macOS build hosts); macOS and Windows (WSL) users are unsupported until that changes.
- Branch protection and required-check configuration on the hosting service is a repository
  setting done by the maintainer, not part of the code.
- The DCO check verifies a `Signed-off-by` trailer on each commit in a pull request.
- Out of scope: Kubernetes, providers, can-core, any module implementation, the site renderer
  and every other epic (E00, E02–E08).
