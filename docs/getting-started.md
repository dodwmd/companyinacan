# Getting started

Three ways to run Company in a Can, from "see it work in five minutes" to "a real server". Be clear about
what each one proves: **only the first gives you the business modules to click through; the last has never run on
a real server.** [status.md](status.md) says, module by module, what works.

| Path | What you get | Needs | Verified |
|---|---|---|---|
| [1. Docker Compose](#1-docker-compose-the-whole-stack-on-one-machine) | Every service behind one Caddy edge, on `localhost`. Dev only, insecure on purpose | Docker with Compose v2 (or podman-compose), about 5 GB disk | End to end, by `make e2e` locally and in the advisory [e2e workflow](../.github/workflows/e2e.yml) |
| [2. k3d](#2-a-local-kubernetes-cluster-with-k3d) | A single-node k3s cluster in Docker with the production base manifests | Docker or Podman, k3d, kubectl | `make dev` itself; CI workflows go further and install, route, back up and restore modules ([status.md](status.md#what-ci-proves-and-when-it-runs)) |
| [3. A real server](#3-a-real-server-make-up) | Provisioning of a VM, k3s, DNS records | A server or a provider account | Offline against fakes, and its deploy phases on k3d in CI; **never run on a real server** |

## 1. Docker Compose: the whole stack on one machine

```sh
make e2e-up        # = docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml up -d --build
make e2e           # the end-to-end suite, about 2 minutes (needs the Go toolchain of go.mod)
make e2e-down      # remove the containers and their volumes
```

`make e2e-browser` runs a second suite against the same stack with a headless Chrome or Chromium that you have
installed (`E2E_CHROME=/path/to/chrome`).

The first build compiles all the Go binaries and takes a few minutes (it needs the network for the Go image,
distroless and Caddy). `make compose-up` starts the same services as project `companyinacan` without the test
overlay, and `make compose-test` runs the older shell smoke test. Details, URLs, the dev credentials and
troubleshooting are in [local-compose.md](local-compose.md).

What is in the stack: the example site, the forms gateway, can-core (admin UI), `work`, `people`, `jobs`,
`finance`, `equity` and `files` services, and the Caddy edge that routes `*.localhost:8080` by host name. `board`,
`entity`, `provisioning` and the wrapped applications are not in it. The test
overlay adds a small fake token-introspection service so that `people` and `equity` can be called by several
identities (HR, a manager, employees, a board member) beside can-core's own tokens.

Things you can do by hand:

1. `docker compose -p can-e2e exec -T -e CAN_ADMIN_PASSWORD=dev-only-admin-password core /can-core admin reset-password --rotate-totp`
   prints a TOTP secret; add it to an authenticator (or compute a code with any RFC 6238 tool), then sign in at
   <http://admin.localhost:8080> as `admin` with the dev password.
2. Fill in the contact form at <http://localhost:8080/contact/>; the lead appears with the dev leads token
   (`curl -H "Authorization: Bearer dev-only-forms-api-token-0123456789abcdef" http://forms.localhost:8080/v1/leads`).
3. Sign in to the tracker at <http://work.localhost:8080/ui/> with the dev work token, create a project and an
   issue, and look at the board.
4. Open <http://jobs.localhost:8080/careers>.
5. In the admin UI, browse **Catalogue**, then under **API tokens** issue a `work` token with `work.read` and use it:
   `curl -H "Authorization: Bearer canpat_..." http://work.localhost:8080/v1/projects`.
6. Sign in at <http://localhost:8180> (the same admin UI), open **Apps** and press **Open** next to a module: you
   are signed in to its web page without pasting a token.

The command in step 1 already names `can-e2e`; give the helper scripts the project the same way, for example
`COMPOSE_PROJECT_NAME=can-e2e dev/compose/bootstrap-admin.sh`. [local-compose.md](local-compose.md#modules-access-tokens-and-single-sign-on)
has the details (which permissions make which role, the directory entry `people` needs, invoice approval).

What compose cannot show (so the e2e suite does not claim it): there is no Kubernetes. can-core records the
modules without installing them and knows their containers by token files, a development mode that release
builds do not contain. Installation, TokenReview, network isolation and TLS are not exercised; a lead does not
become a CRM contact. Those flows are tested with the real code in `test/wiring` (`make unit`) and, for `work`,
on k3d.

## 2. A local Kubernetes cluster with k3d

```sh
make doctor                  # checks docker/podman, k3d and kubectl, with a remedy per missing tool
make dev                     # creates the k3d cluster and applies deploy/base (namespaces, default-deny policies, Traefik, CoreDNS rewrite)
go run ./cmd/can dev down    # deletes it (`make down` needs a company.yaml in the current directory)
```

`make dev` stops at the base manifests: it does not install can-core or any module. The CI workflows build the
images, import them into the cluster and run the integration tests; follow `.github/workflows/dev-cluster.yml`
to reproduce that by hand. See [dev-cluster.md](dev-cluster.md) for flags, exit codes and what has been verified.

## 3. A real server (`make up`)

`make up` provisions (or checks) a server, installs k3s, deploys can-core, your website and the contact form,
creates the admin account and prints how to sign in ([make-up.md](make-up.md)). From a source checkout:

```sh
make build
bin/can init my-company --name "My Co" --domain myco.example --non-interactive   # writes my-company/company.yaml
# edit my-company/company.yaml: provider type ssh, hetzner or digitalocean, with its options (providers.md)
make images                                                  # builds can-core, can-static and can-forms
make up ARGS="my-company --plan" IMAGES_FROM=build/images    # show every phase, change nothing
make up ARGS=my-company IMAGES_FROM=build/images             # do it
make status ARGS=my-company                                  # exit 0 only when the server and stack are up
```

`make up` needs `kubectl` and SSH on your machine. The company directory is an argument: without it the command
looks for `company.yaml` in the current directory. The `provider.type: local` that `can init` writes is not a
`make up` target.

Read these caveats first; they are the honest state of this path:

- **It has never been run against a real server.** The providers are tested against fakes; the deploy phases after
  k3s are exercised on k3d in CI. Server creation, the bootstrap script on a fresh OS, ACME and the five-minute
  target are unverified. If you try it, follow [real-server-verification.md](real-server-verification.md):
  `bin/can verify --server --company-dir my-company` runs the read-only checks.
- **You get the site, the contact form and the admin UI, not the business modules.** Their images are not published
  until a release. **Do not enable them yet**: a module whose image cannot be pulled stops every backup until you
  disable it ([status.md](status.md#known-defects-an-operator-would-hit)).
- **The contact form is not on the generated pages.** It is at `https://<your site>/_forms/contact/form`; link to it
  yourself. The admin UI's Leads page has no credential on an install, so read messages through the notification
  email (needs a relay, [email.md](email.md)) or the forms API.
- Backups are optional (`--backup-endpoint`, `--backup-bucket`, `--backup-credentials-file`, passed in `ARGS`) and
  print a recovery kit once. `can restore` runs on a new server and has been exercised on k3d only. `make up` does
  not copy the `can` binary to the server: put it at `/usr/local/bin/can`, or erasures never reach the bucket
  ([backup-restore.md](backup-restore.md)).
- The 2 GB target is unmeasured; use 4 GB. See the footprint table in the [README](../README.md#footprint-measured-against-claimed).
- SPEC.md section 6.1 sketches `make init`, `make enable <module>`, `make backup` and `make email`; those do not
  exist. Edit `company.yaml` or use the admin UI catalogue, and run `bin/can apply`.
- NetworkPolicy does not isolate pods from the node on k3s. The server bootstrap adds host rules that do
  ([node-isolation.md](node-isolation.md)), unverified on a real server; `make dev` clusters do not have them.
- **Treat the admin password as the factor that matters most.** can-core does not limit sign-in attempts per
  account, so someone who learns the password can keep guessing authentication codes. Keep `CAN_TRUST_PROXY=true`
  behind Traefik (the shipped setting; without it nothing is locked at all), restrict `admin.<domain>` to your own
  addresses if you can, and change the password if the audit log shows `admin.login.many_failures`
  ([security-model.md](security-model.md#known-residual-risks)).

Provider options and what each needs: [providers.md](providers.md). A guide to every document: [index.md](index.md).
