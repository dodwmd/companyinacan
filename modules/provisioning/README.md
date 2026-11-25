# provisioning

In-house identity provisioning module: a SCIM 2.0 server over the people directory and joiner, mover
and leaver workflows against downstream systems. Library `core/provision`, binary `cmd/can-provision`,
image `deploy/provisioning`. It provides the `identity.provision` capability and publishes
`provision.step.completed.v1` and `provision.failed.v1` (ids only).

**Status: experimental.** Built and tested offline against fakes and measured locally (14 MiB idle,
19 MiB after 600 requests). No connector has talked to a real service, no identity provider has used
the SCIM server, the chart installs through can-core's installer in unit tests only, and the default
module-namespace NetworkPolicy does not yet allow the module to reach the people module or any
downstream host. Everything unverified is listed in [`docs/provisioning.md`](../../docs/provisioning.md).

**Installed under can-core** (see [`docs/module-wiring.md`](../../docs/module-wiring.md)): can-core
generates the API token once; personal access tokens with `provision.read`, `provision.write` or
`provision.admin` are introspected through can-core. Workflows start from the people module's signed
events (`person.created.v1`, `person.status_changed.v1`, `person.offboarded.v1`,
`checklist.task_completed.v1`) and are always re-checked against the directory. Privileged access and
the leaving of privileged accounts need a second person through can-core's approvals engine (types
`privileged-access`, `privileged-leaver`; decisions arrive as signed `approval.decided.v1`). The kill
switch for a leaver's can-core tokens and sessions is `POST /v1/identity/revoke` on the module API,
allowed only for a person the people module reported offboarded and for the account linked to them.

Google Workspace, GitHub and Slack are documented stubs that never touch the network.

The guide, with the flows, the SCIM subset, the security model and the honest list of what is
unverified, is [`docs/provisioning.md`](../../docs/provisioning.md). The wire contract is
[`specs/028-identity-provision/contracts/openapi.yaml`](../../specs/028-identity-provision/contracts/openapi.yaml);
the stable cross-module subset is `schemas/capabilities/identity.provision.v1.json`.

Not legal, security or HR advice: offboarding is the operator's responsibility.
