# Research and decisions: E03d

- **R1. `net/smtp`.** Frozen but sufficient (EHLO, STARTTLS, PLAIN, data writer with dot-stuffing).
  `smtp.SendMail` is not used: it upgrades only opportunistically.
- **R2. Credentials bound to host.** `company.yaml` is editable by operators and the admin UI; the
  Secret is not. Sending the Secret's password to a host named only in `company.yaml` would let the
  former exfiltrate the latter.
- **R3. Secret by volume.** can-core has no permission to read Secrets in `can-system` (see SSO), so
  the optional Secret is mounted, which also gives rotation without a restart.
- **R4. Allow-list, not sender trust.** A module token proves who calls, not that the call is
  honest. Recipients are therefore operator configuration. The default includes the company domain so
  a new company works; an explicit list narrows it.
- **R5. One row per recipient.** Independent status and retries, and no recipient list disclosed
  between recipients.
- **R6. At least once.** Exactly-once is not available over SMTP. One worker, one replica
  (`Recreate`), same `Message-ID` on a repeat.
- **R7. Quota refusals answer 429** with `Retry-After`. They limit mail, not authentication.
- **R8. Development identity.** Compose has no TokenReview, so can-core registers fixed module
  identities from `CAN_DEV_MODULE_TOKENS`, refused unless the token review is fake and there is no
  installer. The registered manifest grants `email.send` and nothing else (no permissions, roles or
  subscriptions).
- **R9. Forms settings.** The forms chart ignored module settings, so there was no way to set
  `notify_email` on a cluster. The chart now renders that one setting as a top-level default in
  `forms.yaml`. The other forms settings are still ignored by the chart (existing gap).

- **R10. Vouched addresses, not confirmation links.** A confirmation flow needs its own tokens,
  pages and abuse limits. Restricting to the company domain, an operator list and verified SSO
  claims closes the same hole with configuration only.
- **R11. Fixed text.** Module titles, bodies and event ids are free text; sent from the company's
  address they are a phishing tool. Even the type is spaced out so it cannot be read as a link.
- **R12. Migration numbers.** `0011`, because another open change takes `0010`. The runner applies
  numbers above the highest recorded one, so this must merge after it; a contract test now rejects
  duplicate numbers.

## Unverified

No real relay (SES, Resend, Brevo or other), no cluster (Secret mount, NetworkPolicy egress, module
installation of forms), no `can up` against a server. Deliverability and DNS alignment are untested.
Pending content sits unencrypted in can-core's SQLite file and its backups; blanking uses ordinary
`UPDATE`, so freed pages may keep bytes until reused.
