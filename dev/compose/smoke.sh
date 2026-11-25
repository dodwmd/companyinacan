#!/usr/bin/env bash
# DEV ONLY smoke test for the docker-compose.yml stack. Everything goes through the edge
# (http://*.localhost:8080), exactly as a browser would. Exits non-zero on the first failure.
#
#   dev/compose/smoke.sh                  # uses the public fake dev tokens in dev/compose/secrets/
#   EDGE_PORT=8080 dev/compose/smoke.sh
#
# *.localhost resolves to loopback in browsers and in curl via --resolve, which this script
# uses so it works even where the system resolver does not know *.localhost.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
secrets="$here/secrets"
port="${EDGE_PORT:-8080}"
pass=0

secret() { sed -n "s/^$2=//p" "$secrets/$1.env" | head -n1; }
WORK_TOKEN="${WORK_TOKEN:-$(secret work CAN_WORK_ADMIN_TOKEN)}"
FORMS_TOKEN="${FORMS_TOKEN:-$(secret forms CAN_FORMS_API_TOKEN)}"

resolve=()
for h in localhost admin.localhost work.localhost people.localhost jobs.localhost finance.localhost forms.localhost; do
  resolve+=(--resolve "$h:$port:127.0.0.1")
done
curlx() { curl -sS --max-time 15 "${resolve[@]}" "$@"; }

ok() { pass=$((pass + 1)); printf 'ok   %s\n' "$1"; }
die() {
  printf 'FAIL %s\n' "$1" >&2
  [ $# -gt 1 ] && printf '     %s\n' "${@:2}" >&2
  printf 'Smoke test failed after %d passing checks. Is the stack up? Try: docker compose -p companyinacan ps\n' "$pass" >&2
  exit 1
}

# expect_status <label> <want> <curl args...>
expect_status() {
  local label="$1" want="$2"; shift 2
  local got
  got="$(curlx -o /dev/null -w '%{http_code}' "$@")" || die "$label: curl failed (edge not reachable on port $port?)"
  [ "$got" = "$want" ] || die "$label: expected HTTP $want, got $got" "$*"
  ok "$label (HTTP $got)"
}

# Wait for the edge (a fresh `up` can take a few seconds to settle).
for i in $(seq 1 30); do
  curlx -fso /dev/null "http://localhost:$port/_edge/healthz" && break
  [ "$i" = 30 ] && die "edge did not become reachable at http://localhost:$port/_edge/healthz"
  sleep 2
done
ok "edge healthz"

# 1. site, contact page, form gateway reachable on the same origin
expect_status "site /" 200 "http://localhost:$port/"
expect_status "site /contact/" 200 "http://localhost:$port/contact/"

# 2. form POST creates a lead: fetch the timing token, wait out min_fill_seconds, submit
token="$(curlx "http://localhost:$port/_forms/contact/token" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$token" ] || die "forms: could not fetch a timing token from /_forms/contact/token"
ok "forms timing token"
sleep 2
email="smoke-$(date +%s)@acme.example"
code="$(curlx -o /dev/null -w '%{http_code}' -X POST "http://localhost:$port/_forms/contact" \
  -H "Origin: http://localhost:$port" \
  --data-urlencode "name=Smoke Test" --data-urlencode "email=$email" \
  --data-urlencode "message=hello from dev/compose/smoke.sh" \
  --data-urlencode "website=" --data-urlencode "_t=$token")" || die "forms POST: curl failed"
case "$code" in 200 | 201 | 303) ok "forms POST /_forms/contact (HTTP $code)" ;; *) die "forms POST: expected 201/303, got $code" ;; esac
leads="$(curlx -H "Authorization: Bearer $FORMS_TOKEN" "http://forms.localhost:$port/v1/leads?form=contact")" || die "leads API: curl failed"
case "$leads" in *"$email"*) ok "lead $email visible in the leads API" ;; *) die "lead for $email not found in the leads API" "$leads" ;; esac

# 3. admin UI
expect_status "admin login page" 200 "http://admin.localhost:$port/login"

# 4. work API: create a project and an issue (project may already exist from an earlier run)
auth=(-H "Authorization: Bearer $WORK_TOKEN" -H "Content-Type: application/json")
code="$(curlx -o /dev/null -w '%{http_code}' -X POST "http://work.localhost:$port/v1/projects" "${auth[@]}" \
  -d '{"key":"SMOKE","name":"Smoke test project"}')" || die "work create project: curl failed"
case "$code" in 200 | 201 | 409) ok "work create project (HTTP $code)" ;; *) die "work create project: got HTTP $code" ;; esac
resp="$(curlx -w '\n%{http_code}' -X POST "http://work.localhost:$port/v1/projects/SMOKE/issues" "${auth[@]}" \
  -d '{"type":"task","title":"Created by smoke.sh"}')" || die "work create issue: curl failed"
code="${resp##*$'\n'}"
[ "$code" = 201 ] || die "work create issue: expected 201, got $code" "${resp%$'\n'*}"
ok "work create issue (HTTP 201): $(printf '%s' "${resp%$'\n'*}" | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')"

# 5. healthz of the other modules, careers page
expect_status "people healthz" 200 "http://people.localhost:$port/healthz"
expect_status "jobs healthz" 200 "http://jobs.localhost:$port/healthz"
expect_status "jobs careers page" 200 "http://jobs.localhost:$port/careers"
expect_status "finance healthz" 200 "http://finance.localhost:$port/healthz"
expect_status "work healthz" 200 "http://work.localhost:$port/healthz"

printf '\nSmoke test passed: %d checks.\n' "$pass"
