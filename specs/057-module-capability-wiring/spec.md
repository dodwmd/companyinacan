# Feature 057: capability-based module-to-module connections (E03g)

Status: built, experimental. Unit and contract tested; proven on k3d by the `modules-k3d` workflow
(`test/integration/modules_wiring_k3d_test.go`). Not run on a real server.

## Why

Module namespaces are closed to each other. So on a can-core install `provisioning` could not reach
`people` (it was Ready but could not do its job), and `equity` was never given the verified holder
directory mode of [specs/051](../051-holder-identity/spec.md).

## How it works

A manifest asks for a capability (`requires.capabilities` or `consumes`). can-core connects the module
only if it also has a **grant** for that pair (`core/server/modsecrets.Grants`). **equity's grant also
needs the company to opt in** (below):

| Consumer | Capability | Provider | Credential copied into the consumer | May do |
|---|---|---|---|---|
| `provisioning` (requires) | `directory` | `people` | `CAN_PEOPLE_DIRECTORY_TOKEN` as `CAN_PROVISIONING_DIRECTORY_TOKEN` | `people.read`, `people.write` (specs/048) |
| `equity` (consumes, with `identity: directory`) | `directory` | `people` | `CAN_PEOPLE_LOOKUP_TOKEN` as `CAN_EQUITY_DIRECTORY_TOKEN` | identity lookup only |

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

## equity: directory mode is opt-in

```yaml
modules:
  equity:
    settings:
      identity: directory          # default: attestation
      break_glass_until: "2026-12-01"   # optional, see below
  people: {}
```

- New and existing installations stay in **attestation mode** until `identity: directory` is written.
  Enabling `people`, or upgrading can-core, changes nothing for equity. Validation warns when equity is
  in attestation mode while `people` is enabled (shown by `can validate` and `can apply`), and
  can-equity logs the suggestion at start-up.
- Once set, there is **no silent fallback**. `identity: directory` without an enabled `people` module
  providing the directory is a validation error, so disabling `people` fails until the setting is
  changed back to `attestation` (or equity is disabled). A mistyped value is an error. can-core does not
  install equity without the connection and credential it asked for: if they cannot be made, the install
  or upgrade fails and says so (an upgrade leaves the previous release running).
- Directory mode can **freeze** actions (specs/051): every user who acts in equity needs a directory
  record whose `sso_subject` is their account, set by someone else, otherwise 403 `identity_required`
  (a new directory is empty); holders with a `person_id` are established only by `verify-person`,
  otherwise grants, exercises and ledger entries answer 409; the service account has no person.
  Create and link the records in `people` before opting in. `GET /v1/identity/review` lists what is
  outstanding.
- Plain `can apply` compares the `company.yaml` in the cluster with the new one and prints the change of
  mode and of the break-glass window before syncing; `--dry-run` and the admin UI preview say it on
  equity's step.
- **Break-glass** (specs/051, for a single administrator or recovery): `break_glass_until` becomes
  `CAN_EQUITY_BREAK_GLASS_UNTIL` on can-equity. A date (inclusive, UTC) or RFC 3339 time **at most 7
  days ahead**; further ahead is a validation error. It ends by itself; a past value has no effect.
  Whoever can write `company.yaml` can open it; every use is audited by equity.

## Lifecycle

| Event | Result |
|---|---|
| Consumer and provider enabled (equity: and opted in) | Provider installed first, then the consumer connected |
| Opted in or provider enabled later | The consumer is upgraded and connected; the plan says what changes |
| Consumer disabled | Both policies removed and the credential removed from its Secret |
| Provider disabled | Both policies removed at once. `provisioning`, which requires the directory, keeps its now unusable token so that it still starts. equity in directory mode: refused by validation |
| equity set back to `attestation` | Both policies, the URL and the credential removed |
| Provider fails after being connected | The connection stays; the consumer fails closed |
| Provider never installed successfully | `provisioning` is connected by a later reconcile once it is; equity with the opt-in is not installed without it |
| Provider's token changes (installed afresh) | Copied again (fingerprint in the settings hash) |
| Restore | The connections recorded in the backup are used; a missing credential (a disabled consumer) does not stop the restore; the policies are made by the first normal apply |

The recorded copy of a module's manifest holds its connections (`runtime.Wired`); no database migration.

## Failure behaviour

- **equity, directory unreachable** (people down, policy missing, token refused): every action that
  needs a person and every person link check answers 503 `directory_unavailable`; reads allowed by role
  still work (specs/051 FR-004). Answers about callers are cached for 15 seconds.
- **provisioning, directory unreachable:** the run fails or is retried; no account is created without
  the directory.
- **Admission policy or its RBAC missing** (can-core upgraded without applying `deploy/base`): a
  module that needs a connection is not installed or upgraded; the running release stays.

## Admission

`deploy/base/admission-policy.yaml` gains `can-core-networkpolicies`, for can-core's ServiceAccount in
module namespaces, **on create and update**: a policy not named `can-wire-*` may name only
`kube-system`, `can-system`, `can-data`, its own pods, or an address range that excludes the pod
network; a `can-wire-*` policy must be one of the granted pairs, in the right direction, on the right
port, pod selector to pod selector. It does not cover deletion. `test/contract` keeps its pair list in
step with the grants and the provider's chart. The RBAC grant that lets can-core check the policy exists
is widened by that one name. can-core's other permissions are unchanged (it already manages
NetworkPolicies in module namespaces).

## Threat model

**Assets:** the people directory (who works here, which account is whom), equity's separation of duties.

**Stopped:** a pod of an unconnected module reaching people (default deny on both sides); a pod
borrowing a connected module's label from another namespace; hook or other pods of a connected module;
a module asking for the capability in its manifest without a grant; a chart writing its own
cross-namespace policy; equity's credential writing the directory, listing it, reading names or signing
in; can-core, by bug or compromise, **creating or changing** a NetworkPolicy that opens another
module-to-module path.

**Still possible, and not claimed otherwise:**

1. **A compromised can-core** reads every module Secret, runs any restricted pod in any module
   namespace (including one labelled as a consumer, in the consumer's namespace) and manages those
   Secrets. It can also **delete** NetworkPolicies, `default-deny` included, which opens a namespace's
   traffic: the admission policy covers create and update only, so it is not a barrier against a
   compromised can-core.
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
6. **Whoever can edit `company.yaml`** can set equity back to attestation (the weaker mode), opt into
   directory mode and freeze equity until records are linked, or open a break-glass window of up to 7
   days, again and again. The changes are printed by `can apply` and audited; none needs a second
   person.
7. **NetworkPolicy enforcement** is the cluster's (k3s kube-router); a new pod can have a short window
   before its policies are programmed.

## Not built

- A second person's approval for changes of equity's mode (follows the upgrade approval of the module
  upgrade detection work, once that is merged).
- Connections for other capabilities (`leads` for newsletter and CRM, `ats` for people).
- Rotation and expiry of the copied tokens.
