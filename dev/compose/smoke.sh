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

# 5b. people web UI (E13c): login page, anonymous redirect, a wrong token is refused, API unchanged
expect_status "people UI login page" 200 "http://people.localhost:$port/login"
expect_status "people UI redirects anonymous users to /login" 303 "http://people.localhost:$port/"
expect_status "people UI stylesheet" 200 "http://people.localhost:$port/static/people.css"
jar="$(mktemp)"
curlx -c "$jar" -o /dev/null "http://people.localhost:$port/login"
ui_csrf="$(awk '$6=="can_people_login_csrf"{print $7}' "$jar")"
[ -n "$ui_csrf" ] || die "people UI: no login csrf cookie"
code="$(curlx -b "$jar" -o /dev/null -w '%{http_code}' -X POST "http://people.localhost:$port/login" \
  -H "Sec-Fetch-Site: same-origin" --data-urlencode "csrf=$ui_csrf" --data-urlencode "token=not-a-real-token")" || die "people UI login POST: curl failed"
[ "$code" = 401 ] || die "people UI: a wrong token should get 401, got $code"
ok "people UI refuses a wrong token (HTTP 401)"
rm -f "$jar"
expect_status "people API still answers behind the UI" 401 "http://people.localhost:$port/v1/people"

# 6. OPTIONAL (FINANCE_FLOW=1): the invoice flow. The dev admin token cannot approve an invoice it
# created (separation of duties), so a second token for another subject with the approve scope is
# minted through the tokens API.
if [ "${FINANCE_FLOW:-0}" = 1 ]; then
  FIN_TOKEN="${FIN_TOKEN:-$(secret finance CAN_FINANCE_ADMIN_TOKEN)}"
  fin="http://finance.localhost:$port"
  fa=(-H "Authorization: Bearer $FIN_TOKEN" -H "Content-Type: application/json")
  jfield() { sed -n 's/.*"'"$1"'":"\{0,1\}\([^",}]*\).*/\1/p' | head -n1; }
  party="$(curlx -X POST "$fin/v1/parties" "${fa[@]}" -d '{"kind":"customer","name":"Smoke Customer","email":"customer@acme.example"}' | jfield id)"
  [ -n "$party" ] || die "finance: could not create a party"
  inv="$(curlx -X POST "$fin/v1/invoices" "${fa[@]}" \
    -d "{\"party_id\":$party,\"currency\":\"USD\",\"lines\":[{\"description\":\"Anvil\",\"quantity_milli\":1000,\"unit_price_minor\":4900}]}" | jfield id)"
  [ -n "$inv" ] || die "finance: could not create an invoice"
  ok "finance invoice $inv created (draft)"
  code="$(curlx -o /dev/null -w '%{http_code}' -X POST "$fin/v1/invoices/$inv/approve" "${fa[@]}")"
  { [ "$code" = 403 ] || [ "$code" = 409 ]; } || die "finance: expected the creator's approval to be refused (403/409), got $code"
  ok "finance creator cannot approve own invoice (HTTP $code, separation of duties)"
  appr="$(curlx -X POST "$fin/v1/tokens" "${fa[@]}" \
    -d "{\"kind\":\"service\",\"subject\":\"dev-approver\",\"name\":\"smoke approver $(date +%s)\",\"scopes\":[\"finance.approve\",\"finance.read\"]}" | jfield token)"
  [ -n "$appr" ] || die "finance: could not mint an approver token (scope names: see core/finance/auth.go)"
  code="$(curlx -o /dev/null -w '%{http_code}' -X POST "$fin/v1/invoices/$inv/approve" -H "Authorization: Bearer $appr" -H "Content-Type: application/json")"
  [ "$code" = 200 ] || die "finance: approver token could not approve: HTTP $code"
  ok "finance approve with a second subject (HTTP 200)"
  code="$(curlx -o /dev/null -w '%{http_code}' -X POST "$fin/v1/invoices/$inv/send" "${fa[@]}")"
  [ "$code" = 200 ] || die "finance: send: HTTP $code"
  ok "finance send (HTTP 200)"
fi

printf '\nSmoke test passed: %d checks.\n' "$pass"
