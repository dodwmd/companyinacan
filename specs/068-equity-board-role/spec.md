# 068: the equity board role under can-core, and self service on confirmed records only

**Depends on**: equity (`specs/018`), holder identity (`specs/051`, `specs/057`), users and roles
(`specs/044`), module wiring (`specs/016`), launch tickets (`specs/036`), the equity web UI (`specs/066`).
Refines `SPEC.md` section 3.7 (RBAC, approvals).

## Problems

1. On a `can` installation nobody could approve a grant: approval needs the `board` role, which no can-core
   token or Apps sign-in carried, and neither token introspection nor can-core's approval engine is wired to
   equity there. No grant could reach `approved`.
2. A holder record relinked to another person showed that person the grant, the previous holder's name, email
   and tax id, and let them request and cancel exercises, before anybody had confirmed the new link.

## The board role

The approval stays in the equity service. `SPEC.md` 3.7 names one approval engine for equity grants; its
adapter for equity (`ApprovalEngine`) is still not built, and nothing here removes the seam. This is the
smaller step: can-core's roles already say who the board is (the grant approval's default chain names
`equity:board`), so the module is told.

- **A permission of its own, `equity.board`.** Only the manifest role `board` declares it. The other board
  permissions are a subset of the admin's, which is why a token was never the board before; with
  `equity.board` an administrator cannot assemble the board out of their own permissions, and a token cannot
  be issued with it by somebody who does not hold it.
- A personal access token (introspection) or a launch ticket is the board only with `equity.board`,
  `equity.read`, `equity.approve`, `equity.sensitive-read` and `equity.admin`.
- **Who assigns it.** `equity:board` is assigned on the users pages (`specs/044`), which need a sign-in or
  confirmation of the last 10 minutes. Because an approval chain names the role, only an owner or somebody who
  already holds `equity:board` can assign it, whatever other permissions they have; by default only owners hold
  `rbac.manage`. Removing it ends the person's tokens and sessions.
- **Owners are board members.** The `owner` role holds every permission, this one included. An owner's Apps
  sign-in therefore carries the board role. It does not weaken the rules below, which compare people.
- **Directory mode only.** A can-core credential names no person by itself. Without the directory's link a
  board member could approve their own grant unnoticed, so a can-core credential without a directory person
  may **not** approve or reject (`needsPerson`, now with `grant.approve`). In attestation mode, the default of
  a `can` installation, can-core sign-ins therefore still approve nothing.
- Unchanged: whoever created, edited or submitted a grant cannot approve it; the holder never can; nobody
  administers their own equity; the service account is never the board.
- The web UI asks for a step-up (token again, or a recent admin sign-in) before approval, as before. A token
  used on the API has no step-up, like every other API action; its issue needs an admin session.

### Upgrade

The manifest gains `equity.board` (module 0.3.0), so `can apply` and the admin UI **hold the upgrade for the
operator's approval** (`new permission equity.board`, `role board gains equity.board`). Anybody already
assigned `equity:board` becomes an equity board member when it is applied: check the assignments first.

### What to set up

1. `modules.equity.settings.identity: directory` with the `people` module.
2. A directory record with `sso_subject` for every person who acts in equity, set by somebody else.
3. Roles on the users pages: `equity:hr` for whoever drafts, `equity:board` for directors.
4. A second person confirms each holder record (**Holder checks**).

### Single founder, and small companies

Honestly: **one person cannot approve a grant**, and this change does not let them.

| People | Grant to an employee | Grant to a founder |
|---|---|---|
| One | not possible without a second account for a second person; break-glass lets the service account draft and confirm, and the founder, as owner, approve | not possible at all |
| Two | one drafts and submits, the other approves (the holder record is entered by one and confirmed by the other) | needs break-glass: the service account drafts, the other founder approves (`specs/051`) |
| Three or more | normal | one drafts, a second approves, the third is the holder |

Two accounts of one human are not two people; the directory links one account per person, and whoever can edit
the directory or create accounts can defeat that (`specs/051`, threat model 4). An owner can assign roles and
create accounts, so these rules do not bind a dishonest owner; the audit log records what they did.

## Self service needs a confirmed record

Every self-service path (own grants, vesting, identity, holder record, exercise list, request and cancel; API
and web UI) needs a record linked to the caller **and confirmed by a second actor**. A relink, a changed
identity or an upgrade that cleared confirmations hides the record from the person it names until it is
confirmed again; staff still see it by role. Separation of duties still compares the recorded `person_id`
whether or not it is confirmed. This replaces "reads are unchanged" for holders in `specs/051` FR-010.

Also:

- A grant cancelled before it was approved is never shown to its holder.
- The web UI's "Confirm the person link" posts the person id it showed and is refused if the record was
  relinked since. It shows the directory's name for the person when the directory credential gets names;
  can-core's lookup token does not (it is deliberately narrow), and the page then says to look the id up.
- The API's `verify-person` is unchanged: it confirms the link as it is when the call arrives.
- Exercise lists are paged (500, `next_cursor`) instead of being cut off.
- The grants export is always the current state: its file name and audit entry no longer carry an "as of"
  date.

## Not done

- The can-core approval engine adapter (chains, delegation, notifications for grant approval).
- Exercise approval is unchanged: `finance`, `board` or `admin`.

## Success criteria

- `core/equity`: a relinked record shows the new person nothing until confirmed (`relink_test.go`); board
  approval with can-core credentials and every refusal (`canauth_test.go`).
- `core/server/rbac`: only an owner (or a holder) assigns `equity:board`. `core/server/reconcile`: the upgrade
  is held.
- `test/e2e/equity_board_test.go`: on the Compose stack with real can-core roles, tokens and Apps sign-in and
  the people directory (service `equity-core`), HR drafts and a director approves, through the API and the web
  UI; nobody else can. The e2e job is advisory.
