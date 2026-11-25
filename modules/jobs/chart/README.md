# jobs chart

`values.yaml` documents the contract the service expects (port, secrets, keyring mount, probes,
security context). The Helm templates arrive with the module deployment work; until then the module
is validated by `can validate` and run directly with `cmd/can-jobs`.
