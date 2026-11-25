# forms chart

`values.yaml` documents the contract the service expects (ports, secrets, probes, security
context). The Helm templates arrive with the module deployment epic (E03); until then the
module is validated by `can validate` and run directly with `cmd/can-forms`.
