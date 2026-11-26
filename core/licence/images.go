package licence

// Image is a container image that runs in a company's cluster and is not simply the wrapped
// application as its authors publish it: a third party's packaging of one, or infrastructure that is
// no module. The module table cannot say that, so these are listed by hand;
// test/contract/images_notice_test.go keeps Ref in step with what the charts and templates deploy.
type Image struct {
	Ref     string // repository:tag@digest, as deployed
	Used    string // what uses it
	What    string
	Licence string
	Source  string
}

// Images are the entries of the "Container images" table of THIRD_PARTY_LICENSES.
var Images = []Image{
	{
		Ref:     "docker.io/pikapods/docker-freescout:1.8.252-r2@sha256:fcb46451971ae10046e4008f036203f64fd0dc6f637cfd45ab0c9ee31ccdebfa",
		Used:    "module support",
		What:    "FreeScout 1.8.252 packaged by PikaPods (a third party, not the FreeScout project) on serversideup/php",
		Licence: "AGPL-3.0 as the packaging's README states (its repository has no licence file); base image project serversideup/docker-php is GPL-3.0; the image also holds PHP, nginx and Alpine packages under their own licences",
		Source:  "https://github.com/pikapods/docker-freescout",
	},
	{
		Ref:     "docker.io/library/postgres:17.12-alpine@sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73",
		Used:    "can-core (the shared database server in can-data)",
		What:    "PostgreSQL 17 server, Docker official image",
		Licence: "PostgreSQL Licence (server); MIT (image build scripts, docker-library/postgres); Alpine packages under their own licences",
		Source:  "https://github.com/docker-library/postgres",
	},
}
