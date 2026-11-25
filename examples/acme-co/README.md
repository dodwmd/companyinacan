# acme-co

Example company repository (fake data). Validate with:

```sh
can validate examples/acme-co --modules-dir modules
```

Render and preview its website (the `site:` section of `company.yaml`):

```sh
can site build examples/acme-co --out /tmp/acme-site
can site serve examples/acme-co        # http://127.0.0.1:8080, rebuilds on edit
```
