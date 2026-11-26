// Package acceptance holds the whole-journey test of the product promise (specs/058-acceptance-journey):
// from the built `can` binary, the built images and an empty directory to a live HTTPS site, a contact
// form whose notification arrives, an admin with TOTP, modules, a backup, a destroyed server and a
// restore. The test itself has the build tag `acceptance` and only runs on a throwaway machine that
// setup-runner.sh prepared (.github/workflows/acceptance.yml). Never run it on a computer you use:
// it installs k3s on it and destroys it.
package acceptance
