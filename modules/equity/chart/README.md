# equity chart

`values.yaml` documents the contract the service expects (ports, secrets, keyring mount, probes,
security context, network policy intent). The Helm templates arrive with the module deployment
epic (E03); until then the module is validated by `can validate` and run directly with
`cmd/can-equity`. The service has no public listener and no ingress: only the ClusterIP API
on :8081.
