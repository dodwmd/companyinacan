# Getting started

Three ways to run Company in a Can, from "see it work in five minutes" to "a real server". Be clear about
what each one proves: **only the first has been run end to end; the last has never run on a real server.**

| Path | What you get | Needs | Verified |
|---|---|---|---|
| [1. Docker Compose](#1-docker-compose-the-whole-stack-on-one-machine) | Every service behind one Caddy edge, on `localhost`. Dev only, insecure on purpose | Docker with Compose v2 (or podman-compose), about 5 GB disk | End to end, by `make e2e` locally and in the advisory [e2e workflow](../.github/workflows/e2e.yml) |
| [2. k3d](#2-a-local-kubernetes-cluster-with-k3d) | A single-node k3s cluster in Docker with the production base manifests | Docker or Podman, k3d, kubectl | Cluster, network policies and one module (`work`) installed through can-core, in the advisory [dev-cluster workflow](../.github/workflows/dev-cluster.yml) |
| [3. A real server](#3-a-real-server-make-up) | Provisioning of a VM, k3s, DNS records | A server or a provider account | Offline against fakes only; **never run on a real server** |

## 1. Docker Compose: the whole stack on one machine

```sh
make e2e-up        # = docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml up -d --build
make e2e           # the end-to-end suite, about 30 seconds
make e2e-down      # remove the containers and their volumes
```

The first build compiles nine Go binaries and takes a few minutes (it needs the network for the Go image,
distroless and Caddy). `make compose-up` starts the same services as project `companyinacan` without the test
overlay, and `make compose-test` runs the older shell smoke test. Details, URLs, the dev credentials and
troubleshooting are in [local-compose.md](local-compose.md).

What is in the stack: the example site, the forms gateway, can-core (admin UI), `work`, `people`, `jobs`,
`finance` and `equity` services, and the Caddy edge that routes `*.localhost:8080` by host name. The test
overlay adds a small fake token-introspection service so that `people` and `equity` can be called by several
identities (HR, a manager, employees, a board member); without it those two services accept one static token.

Things you can do by hand:

1. `docker compose -p can-e2e exec -T -e CAN_ADMIN_PASSWORD=dev-only-admin-password core /can-core admin reset-password --rotate-totp`
   prints a TOTP secret; add it to an authenticator (or compute a code with any RFC 6238 tool), then sign in at
   <http://admin.localhost:8080> as `admin` with the dev password.
2. Fill in the contact form at <http://localhost:8080/contact/>; the lead appears with the dev leads token
   (`curl -H "Authorization: Bearer dev-only-forms-api-token-0123456789abcdef" http://forms.localhost:8080/v1/leads`).
3. Sign in to the tracker at <http://work.localhost:8080/ui/> with the dev work token, create a project and an
   issue, and look at the board.
4. Open <http://jobs.localhost:8080/careers>.

What compose cannot show (so the e2e suite does not claim it): there is no Kubernetes, so can-core runs with
`CAN_INSTALLER=none`. The module catalogue answers "not available", no module is installed, and therefore no
personal access token can be issued or verified by a module; modules are not wired to each other, so a lead does
not become a CRM contact and a hired candidate does not become a person. Those flows are tested with the real
code in `test/wiring` (`make unit`) and, for `work`, on k3d.

## 2. A local Kubernetes cluster with k3d

```sh
make doctor        # checks docker/podman, k3d and kubectl, with a remedy per missing tool
make dev           # creates the k3d cluster and applies deploy/base (namespaces, default-deny policies, Traefik, CoreDNS rewrite)
make down          # deletes it
```

`make dev` stops at the base manifests: it does not install can-core or any module. The CI job builds the
can-core and `work` images, imports them into the cluster and runs the integration tests; follow
`.github/workflows/dev-cluster.yml` to reproduce that by hand. See [dev-cluster.md](dev-cluster.md) for flags,
exit codes and what has been verified (and not: ACME, `externalTrafficPolicy: Local`, Podman runs).

## 3. A real server (`make up`)

```sh
bin/can init my-company --name "My Co" --domain myco.example --non-interactive   # writes company.yaml
# edit company.yaml: provider (ssh, hetzner or digitalocean), dns, modules
make up ARGS=--plan     # show what would happen, change nothing
make up                 # provision, bootstrap k3s, print the address and DNS records
```

Read these caveats first; they are the honest state of this path:

- **It has never been run against a real server.** The providers are tested against fakes (`httptest`, a fake
  ssh) and the bootstrap script has been syntax-checked, not executed on a fresh Ubuntu host.
- **`make up` does not yet install can-core or the site.** After it succeeds you have a bare k3s with the base
  manifests and the DNS records to add, so the instant URL serves nothing yet ([make-up.md](make-up.md)).
- The recovery kit and backups to object storage are implemented offline; `can restore` stops at step 1 with
  exit code 3 until the provisioning adapters ship ([backup-restore.md](backup-restore.md)).
- The 2 GB target is unmeasured; use 4 GB. See the footprint table in the [README](../README.md#footprint-measured-against-claimed).
- SPEC.md section 6.1 sketches `make init`, `make enable <module>`, `make backup` and `make email`; those are
  not Makefile targets today. Edit `company.yaml` (or use the admin UI wizard once can-core is installed on a
  cluster) and run `can apply`.
- NetworkPolicy does not isolate pods from the node on k3s. The server bootstrap adds host rules that do
  ([node-isolation.md](node-isolation.md)), unverified on a real server; `make dev` clusters do not have them.

Provider options and what each needs: [providers.md](providers.md). A guide to every document: [index.md](index.md).
