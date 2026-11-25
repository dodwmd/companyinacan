# Security Policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Report it privately with GitHub's private vulnerability reporting: on the repository page choose
**Security → Report a vulnerability**. If that is unavailable, contact the repository owner
(dodwmd) directly on GitHub and ask for a private channel before sharing details.

Include what you found, how to reproduce it, the affected version or commit, and the impact you
expect. Please do not include real credentials or personal data in the report.

## What to expect

- We aim to acknowledge a report within **7 days** and to share an assessment or a plan within
  **30 days**. This is a volunteer-run, pre-alpha project, so these are goals, not guarantees.
- We will credit you in the fix notes unless you prefer to stay anonymous.
- Please give us reasonable time to fix the problem before disclosing it publicly.

## Scope

In scope: code, charts, scripts and configuration in this repository, including the `can` CLI,
and the module manifests and hooks we ship.

Out of scope: vulnerabilities in third-party applications that modules wrap (report those
upstream; tell us if our configuration makes them worse), and findings that require an
already-compromised host.

## Supported versions

The project has no stable release yet. Only the `main` branch is supported.

## Security design

The security model (default-deny network policies, encrypted secrets, least privilege, sensitive
data handling, audit and retention) is described in [SPEC.md](SPEC.md) §3.1, §3.7 and §5 and in
the project [constitution](.specify/memory/constitution.md) (Principle V).
