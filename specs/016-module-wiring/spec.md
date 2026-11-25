# Feature Specification: Module wiring (Epic E03c)

**Feature Branch**: `epic/E03c-module-wiring`  |  **Status**: built offline, advisory k3d check for `work`

**Refines**: SPEC.md section 3.1 (module system, install flow), 3.7 (shared platform services), 4.3
(module API, introspection, events). Contradicts nothing there; it fills in what the in-house modules
(forms, work, people, jobs, finance) needed to be installable and to talk to each other through
can-core.

## Problem

The five in-house modules were built as libraries with a service wrapper each, tested alone. They had
`values.yaml` but no Helm templates, so can-core could not install them. Their authentication seams
(`Authenticator`) still read static tokens; nothing issued tokens to people; the secrets they need at
start (admin tokens, HMAC keys, sealed-field keyrings) were created by hand; events went to one
configured webhook URL instead of through can-core; and approvals (jobs requisitions, finance
invoices) used placeholder approvers.

## User scenarios

1. **Install.** An operator sets `modules.work.enabled: true` and runs `can apply`. can-core renders the
   chart, creates the namespace, the per-module Secret (including the secrets the module needs) and the
   workloads, and the module starts and passes its probes.
2. **Use with my own token.** A person signs in to the admin UI, opens "API tokens", picks a module and
   the permissions they hold, gets a token shown once, and calls the module's API with it. They can
   revoke it. The module never sees the person's password or session.
3. **Rotate.** An operator runs `can-core admin secrets rotate <module> <key>`; the next reconcile
   regenerates the value and rolls the module. Nothing is printed or logged.
4. **A lead notifies sales.** A form submission publishes `lead.created.v1` through can-core; the
   `sales` role gets an inbox item. No name, e-mail address or message text leaves the forms database.
5. **A hire starts onboarding.** Jobs publishes `candidate.hired.v1`; can-core delivers it signed to
   people; people creates a person in onboarding with the onboarding checklist, once per candidate.
6. **Approvals.** A recruiter asks to approve a requisition, an accountant to approve an invoice; can-core's
   engine decides (chain, separation of duties); the module applies the decision. Nobody approves their
   own request.
7. **Leave stays private.** A person going on leave is `active` in every event.

## Requirements

- **FR-1 Charts.** Each module has Helm-compatible templates (Deployment with `Recreate` for SQLite,
  ClusterIP Service, PVC kept on uninstall, optional same-namespace NetworkPolicy, ConfigMap built from
  values, an IngressRoute only when the manifest declares a `subdomain` and the installer supplies the
  host), restricted security context, resources equal to the manifest's `ram_mb`/`ram_limit_mb`, probes
  using the binary's own `healthcheck`. They install through the installer's allowlist. Image digests
  are placeholders that `make release-check` rejects.
- **FR-2 Tokens.** can-core issues personal access tokens (`canpat_...`) scoped to one module and to a
  subset of the permissions that module declares, limited to what the issuer holds, hashed at rest,
  expiring (1 hour to 365 days), revocable, capped per module. `POST /v1/tokens/introspect` answers a
  module for its own tokens only (subject and permissions), `{"active": false}` for everything else,
  with the same body for every failure.
- **FR-3 Module side.** Each module has an `Authenticator` that asks can-core, caches boundedly, never
  caches errors, never throttles a valid token, and refuses everything when can-core cannot answer.
  The static bootstrap token keeps working without can-core.
- **FR-4 Secrets.** can-core creates each module's secrets once (generate-once, kept across enable,
  upgrade and disable), puts them in the module's `can-module` Secret, never logs or prints them, rolls
  the module when they change, and rotates them on request. Keys whose rotation would silently damage
  data are refused.
- **FR-5 Events.** Modules publish through `POST /v1/events` (idempotent on the publisher's event id).
  can-core delivers to subscribers as signed webhooks; modules verify signature and replay window and
  ignore redeliveries. Module-to-module traffic goes through can-core only.
- **FR-6 Approvals.** Jobs requisitions and finance invoices use the approvals engine behind an interface
  (`modclient.Approvals`); the decision arrives as `approval.decided.v1`. Separation of duties is kept in
  three places: the engine, the module's own check of who requested, and (finance) the approver of the
  invoice not being its creator, editor or requester.
- **FR-7 CI.** The advisory dev-cluster workflow builds the `work` image, installs it through can-core on
  k3d, creates an issue with a personal access token, disables the module and checks the volume.
- **FR-8 Footprint.** can-core idle RSS stays within its declared budget; module idle RSS grows by
  well under 1 MiB.

## Out of scope / known gaps

Listed in `docs/module-wiring.md` ("What is not done") and in [tasks.md](tasks.md).
