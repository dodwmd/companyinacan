# Feature 057: capability-based module-to-module connections (E03g)

Status: built, experimental. Unit and contract tested; proven on k3d by the `modules-k3d` workflow
(`test/integration/modules_wiring_k3d_test.go`). Not run on a real server.

## Why

Module namespaces are closed to each other. So on a can-core install `provisioning` could not reach
`people` (it was Ready but could not do its job), and `equity` was never given the verified holder
directory mode of [specs/051](../051-holder-identity/spec.md).

## How it works

A manifest asks for a capability (`requires.capabilities` or `consumes`). can-core connects the module
only if it also has a **grant** for that pair (`core/server/modsecrets.Grants`):

| Consumer | Capability | Provider | Credential copied into the consumer | May do |
|---|---|---|---|---|
| `provisioning` (requires) | `directory` | `people` | `CAN_PEOPLE_DIRECTORY_TOKEN` as `CAN_PROVISIONING_DIRECTORY_TOKEN` | `people.read`, `people.write` (specs/048) |
| `equity` (consumes) | `directory` | `people` | `CAN_PEOPLE_LOOKUP_TOKEN` as `CAN_EQUITY_DIRECTORY_TOKEN` | identity lookup only |

The grant list is can-core's, not the manifests': a manifest that names a capability gets nothing
without a grant. Another provider of `directory`, or a different designated `primary`, gets no
connection.

A connection is:

- `NetworkPolicy mod-<consumer>/can-wire-to-<provider>`: egress from the consumer's app pods
  (`app.kubernetes.io/name`) to the provider's app pods, on the provided port, TCP only;
- `NetworkPolicy mod-<provider>/can-wire-from-<consumer>`: the matching ingress;
- the provider's URL in the consumer's chart `config.directory` (`http://people-api.mod-people.svc.cluster.local:8081`);
- the credential in the consumer's `can-module` Secret.

Hook Jobs and other pods of either namespace are not admitted. A chart may not define a `can-wire-*`
policy.

### The lookup credential

`CAN_PEOPLE_LOOKUP_TOKEN` is new and has no role. It answers only:

- `GET /v1/people?sso_subject=<exact>`: `{items: [{id, sso_subject}]}` or an empty list;
- `GET /v1/people/{id}`: `{id}`, with `erased: true` for an erased record.

Everything else is 403: no list, no names or contact data, no writes, no web sign-in. This replaces the
`people.read` and `people.write` token specs/051 asked for.

## Lifecycle

| Event | Result |
|---|---|
| Both enabled | Provider installed first, then the consumer connected |
| Provider enabled later | The consumer is upgraded and connected; the plan says what changes |
| Consumer disabled | Both policies removed and the credential removed from its Secret |
| Provider disabled | Both policies removed at once. An optional consumer is then upgraded without the URL and credential. `provisioning`, which requires the directory, keeps its now unusable token so that it still starts |
| Provider fails after being connected | The connection stays; the consumer fails closed |
| Provider never installed successfully | No connection; the consumer is connected by a later reconcile once it is |
| Provider's token changes (installed afresh) | Copied again (fingerprint in the settings hash) |
| Restore | The connections recorded in the backup are used; the policies are made by the first normal apply |

The recorded copy of a module's manifest holds its connections (`runtime.Wired`); no database migration.

## Failure behaviour

- **equity, directory unreachable** (people down, policy missing, token refused): every action that
  needs a person and every person link check answers 503 `directory_unavailable`; reads allowed by role
  still work (specs/051 FR-004). Answers about callers are cached for 15 seconds.
- **provisioning, directory unreachable:** the run fails or is retried; no account is created without
  the directory.
- **Admission policy or its RBAC missing** (can-core upgraded without applying `deploy/base`): a
  module that needs a connection is not installed or upgraded; the running release stays.

## Turning people on while equity is already installed

Enabling `people` moves equity from attestation to directory mode (specs/051). Nothing is deleted and
nothing becomes newly allowed, but equity can **freeze**:

- every user who acts in equity needs a directory record whose `sso_subject` is their account, set by
  someone else, otherwise 403 `identity_required`. A new directory is empty;
- holders with a `person_id` are established only by `verify-person` (attestations made without a
  directory do not count), otherwise grants, exercises and ledger entries answer 409;
- the service account has no person (break-glass in specs/051 is the way through for one administrator).

The plan (`can apply`, the admin UI's change preview) says so on equity's step. **It is not held for
approval in this change**: it happens when the change is applied. Before enabling `people`, know who
will create and link the records; `GET /v1/identity/review` lists what is outstanding. Disabling
`people` again returns equity to attestation mode, which checks less; the plan says that too.

An existing installation that already has both modules gets the same switch at its first apply after
upgrading can-core.

## Admission

`deploy/base/admission-policy.yaml` gains `can-core-networkpolicies`, for can-core's ServiceAccount in
module namespaces: a policy not named `can-wire-*` may name only `kube-system`, `can-system`,
`can-data`, its own pods, or an address range that excludes the pod network; a `can-wire-*` policy must
be one of the granted pairs, in the right direction, on the right port, pod selector to pod selector.
`test/contract` keeps its pair list in step with the grants and the provider's chart. The RBAC grant
that lets can-core check the policy exists is widened by that one name. can-core's other permissions are
unchanged (it already manages NetworkPolicies in module namespaces).

## Threat model

**Assets:** the people directory (who works here, which account is whom), equity's separation of duties.

**Stopped:** a pod of an unconnected module reaching people (default deny on both sides); a pod
borrowing a connected module's label from another namespace; hook or other pods of a connected module;
a module asking for the capability in its manifest without a grant; a chart writing its own
cross-namespace policy; equity's credential writing the directory, listing it, reading names or signing
in; a compromised can-core opening other module-to-module paths through NetworkPolicies.

**Still possible, and not claimed otherwise:**

1. **A compromised can-core** reads every module Secret, runs any restricted pod in any module
   namespace (including one labelled as a consumer, in the consumer's namespace) and manages those
   Secrets. The admission policy narrows network paths, not that.
2. **A compromised provisioning** can read and change the directory (including `sso_subject`), which
   decides who equity believes a caller is. Its token has `people.write` because SCIM PATCH and
   checklists need it. It also has the internet egress its connectors need.
3. **A compromised equity** can ask, for any account name it can guess, which person it is. It cannot
   list accounts or people.
4. **The tokens do not expire and are not rotated** by `secrets rotate`. A leaked token is usable only
   from an admitted pod, the platform pods that may reach people, or (the API is not routed publicly by
   default) wherever an operator exposes the people API.
5. **Plain HTTP in the cluster.** Tokens cross the pod network unencrypted; someone who can observe node
   traffic can read them.
6. **Whoever can edit `company.yaml`** can disable `people` and so return equity to the weaker mode, or
   enable it and freeze equity until records are linked. Both are shown in the plan and audited.
7. **NetworkPolicy enforcement** is the cluster's (k3s kube-router); a new pod can have a short window
   before its policies are programmed.

## Not built

- Holding the mode switch for approval (follows the upgrade approval of the module upgrade detection
  work, once that is merged).
- Connections for other capabilities (`leads` for newsletter and CRM, `ats` for people).
- Rotation and expiry of the copied tokens.
