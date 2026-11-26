# Feature 064: module settings and company facts reach the modules (E03h)

Status: built. Unit tested with the real chart renderer against a fake Kubernetes API, each rendered
configuration read by the module's own loader. The k3d workflow `modules-k3d` installs every in-house module
with settings set and reads one module's ConfigMap. Never run on a real server.

## Why

`modules.<id>.settings` in `company.yaml` mostly did not reach the installed module. finance never got the
seller or `payment_instructions`, so invoices named the web address instead of the company; retention, policy
and session settings could not be set on a real install; settings of wrapped modules were accepted and ignored.

## Requirements

- **Declarative mapping.** Every leaf property of a shipped module's `settings.schema.json` carries `x-can`,
  with exactly one of `config` (a key of the module's configuration file), `value` (a chart value under
  `app.`), `handled` (read by another part of can-core or the chart) and `not_applied` (why it reaches
  nothing). A contract test fails for a property without it. The loader refuses unknown annotation keys,
  targets can-core owns (listeners, paths, UI switches, public origins, webhooks, connectors) and secret
  names.
- **Company facts.** `company.yaml` gains an optional `organisation` mapping: `legal_name`, `address`,
  `tax_id`, `currency`, `timezone`. A module setting wins over a fact; a fact wins over the module's default.
  can-core's own values (connections, routing, public origins) win over both.
- **Never silently ignored.** An unknown setting, an invalid value, a setting that is not applied and a key
  that reads like a secret are warnings (`semantic.setting-unknown`, `-invalid`, `-not-applied`, `-secret`,
  `semantic.organisation`) from `can validate` and `can apply`, and are listed in the admin UI. They are
  warnings so that a `company.yaml` that validated before still does. (The refusals that existed before, such
  as a misspelt equity `identity`, stay errors.) A message never quotes the value.
- **Not applied means default.** An invalid value is left out: the module runs with the fact or its default.
- **Secrets never come from settings.**
- **Upgrades.** The mapped values are part of what an installation is made of: changing a setting or a fact,
  or a release that maps one differently, plans an upgrade.
- **Approval.** An upgrade waits for approval (as in PR #77) when the new values widen what the module does,
  exposes or destroys compared with what it was installed with (its defaults where nothing was mapped).
  The list is the `widens` annotations: shorter retention, relaxed protections (virus scan, dry run,
  approvals, anonymity threshold, authentication and request limits, session length), who is mailed, what
  applicants consent to, when and with which tasks leavers are processed, more exposure (CORS origins, inline
  images, trusted proxy headers), diversity data collection, a changed reporting currency or invoice link
  base, development providers. Returning to a stricter value while held is reported as urgent.
  A fresh install is not held, as for everything else, and neither is enabling a disabled module again (the
  plan lists what it newly allows).
- **What is approved is what is shown.** One line per new value or per entry a list gains or loses, ending in a
  digest of the whole value; the approval identifier covers those lines.
- **Restore.** A restore, and the apply that follows it, install nothing wider than the record in the backup
  (`Applied.values`; the module's default where there is none), record that, and leave the difference to the next
  plan, which holds it. The modules start all the same.
- **Earlier releases.** `forms.notify_email` was already applied: unchanged settings of such an installation do not
  count as new recipients. A record from before `Applied` existed cannot tell, and is held.
- **Admin UI.** The settings form shows nested settings, lists and multi-line text, lists what is not applied
  with the reason, and does not write a default into `company.yaml` that was only shown.

## After upgrading can-core

- An installed finance gets `seller.name` (the company name until `organisation.legal_name` is written) and
  is upgraded once.
- A setting that was written long ago and ignored now takes effect. One that widens waits for approval.

## Not applied

See [docs/module-wiring.md](../../docs/module-wiring.md#settings). In short: the forms definitions
themselves, `webhook_url` and `captcha_provider` (they need a secret can-core does not provision), and most
settings of the wrapped applications, which are configured inside the application.

## Limits

- Values are not checked against data already stored: changing `base_currency` after the first document is
  held for approval, not refused.
- The k3d run proves the pods start with the mapped configuration, not that each setting behaves.
