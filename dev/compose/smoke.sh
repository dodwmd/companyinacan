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
for h in localhost admin.localhost work.localhost people.localhost jobs.localhost finance.localhost forms.localhost equity.localhost files.localhost mail.localhost; do
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

# 1. site, its link to the contact form, form gateway reachable on the same origin
expect_status "site /" 200 "http://localhost:$port/"
case "$(curlx "http://localhost:$port/")" in *'href="/_forms/contact/form"'*) ok "site links to the contact form" ;; *) die "the landing page does not link to /_forms/contact/form" ;; esac
expect_status "contact form page" 200 "http://localhost:$port/_forms/contact/form"

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

# 2b. the lead notification reaches the SMTP sink through email.send (can-core's outbox)
found=""
for _ in $(seq 1 30); do
  case "$(curlx "http://mail.localhost:$port/messages")" in *"$email"*) found=1; break ;; esac
  sleep 1
done
[ -n "$found" ] || die "no notification email for $email in the SMTP sink (see the logs of core and forms)"
ok "lead notification email delivered to the SMTP sink"

# 3. admin UI
expect_status "admin login page" 200 "http://admin.localhost:$port/login"
# 3b. the one-host-name address the Apps launcher needs (plain http), default stack ports only
if [ "$port" = 8080 ]; then
  expect_status "admin login page on localhost:8180" 200 "http://localhost:8180/login"
fi

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

# 4b. work web UI: sign in by pasting the dev token (cookie jar, like a browser), open the project
# list and the project's backlog, and check that an anonymous request is sent to the login page.
work_ui="http://work.localhost:$port/ui"
jar="$(mktemp)"
trap 'rm -f "$jar"' EXIT
expect_status "work UI redirects anonymous users to sign-in" 303 "$work_ui/"
login="$(curlx -c "$jar" -b "$jar" "$work_ui/login")" || die "work UI login page: curl failed"
csrf="$(printf '%s' "$login" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -n1)"
[ -n "$csrf" ] || die "work UI: no CSRF field on the login page"
code="$(curlx -o /dev/null -w '%{http_code}' -c "$jar" -b "$jar" -X POST "$work_ui/login" \
  -H "Origin: http://work.localhost:$port" --data-urlencode "csrf=$csrf" --data-urlencode "token=$WORK_TOKEN")" || die "work UI sign-in: curl failed"
[ "$code" = 303 ] || die "work UI sign-in: expected HTTP 303, got $code"
ok "work UI sign-in (HTTP 303)"
page="$(curlx -b "$jar" "$work_ui/")" || die "work UI project list: curl failed"
case "$page" in *"/ui/projects/SMOKE"*) ok "work UI lists the SMOKE project" ;; *) die "work UI project list lacks SMOKE" "$page" ;; esac
page="$(curlx -b "$jar" "$work_ui/projects/SMOKE")" || die "work UI backlog: curl failed"
case "$page" in *"Created by smoke.sh"*) ok "work UI backlog shows the issue created through the API" ;; *) die "work UI backlog lacks the smoke issue" ;; esac
session_csrf="$(printf '%s' "$page" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -n1)"
code="$(curlx -o /dev/null -w '%{http_code}' -b "$jar" -X POST "$work_ui/logout" --data-urlencode "csrf=$session_csrf")" || die "work UI logout: curl failed"
[ "$code" = 303 ] || die "work UI logout: expected HTTP 303, got $code"
expect_status "work UI session ended by logout" 303 -b "$jar" "$work_ui/"

# 5. healthz of the other modules, careers page
expect_status "people healthz" 200 "http://people.localhost:$port/healthz"
expect_status "jobs healthz" 200 "http://jobs.localhost:$port/healthz"
expect_status "jobs careers page" 200 "http://jobs.localhost:$port/careers"
expect_status "finance healthz" 200 "http://finance.localhost:$port/healthz"
expect_status "work healthz" 200 "http://work.localhost:$port/healthz"
expect_status "equity healthz" 200 "http://equity.localhost:$port/healthz"
expect_status "equity API refuses a request without a token" 401 "http://equity.localhost:$port/v1/entities"
expect_status "files healthz" 200 "http://files.localhost:$port/healthz"
expect_status "files API refuses a request without a token" 401 "http://files.localhost:$port/v1/spaces"
expect_status "files share route answers 404 for a made-up link" 404 "http://files.localhost:$port/s/shr_000000000000000000000000.1.AAAA"
expect_status "files web UI sign-in page" 200 "http://files.localhost:$port/login"
expect_status "unknown Host header gets nothing (421)" 421 -H "Host: evil.example" "http://localhost:$port/"

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

# 5c. jobs web UI (E16b): login page, anonymous redirect, a wrong token is refused, careers and API unchanged
expect_status "jobs UI login page" 200 "http://jobs.localhost:$port/login"
expect_status "jobs UI redirects anonymous users to /login" 303 "http://jobs.localhost:$port/"
expect_status "jobs UI stylesheet" 200 "http://jobs.localhost:$port/static/jobs.css"
jar="$(mktemp)"
curlx -c "$jar" -o /dev/null "http://jobs.localhost:$port/login"
jobs_csrf="$(awk '$6=="can_jobs_login_csrf"{print $7}' "$jar")"
[ -n "$jobs_csrf" ] || die "jobs UI: no login csrf cookie"
code="$(curlx -b "$jar" -o /dev/null -w '%{http_code}' -X POST "http://jobs.localhost:$port/login" \
  -H "Sec-Fetch-Site: same-origin" --data-urlencode "csrf=$jobs_csrf" --data-urlencode "token=not-a-real-token")" || die "jobs UI login POST: curl failed"
[ "$code" = 401 ] || die "jobs UI: a wrong token should get 401, got $code"
ok "jobs UI refuses a wrong token (HTTP 401)"
rm -f "$jar"
expect_status "jobs API still answers behind the UI" 401 "http://jobs.localhost:$port/v1/jobs"

# 5d. finance web UI (E15b): login page, anonymous redirect, a wrong token is refused, API unchanged
expect_status "finance UI login page" 200 "http://finance.localhost:$port/login"
expect_status "finance UI redirects anonymous users to /login" 303 "http://finance.localhost:$port/"
expect_status "finance UI stylesheet" 200 "http://finance.localhost:$port/static/finance.css"
jar="$(mktemp)"
curlx -c "$jar" -o /dev/null "http://finance.localhost:$port/login"
fui_csrf="$(awk '$6=="can_finance_login_csrf"{print $7}' "$jar")"
[ -n "$fui_csrf" ] || die "finance UI: no login csrf cookie"
code="$(curlx -b "$jar" -o /dev/null -w '%{http_code}' -X POST "http://finance.localhost:$port/login" \
  -H "Sec-Fetch-Site: same-origin" --data-urlencode "csrf=$fui_csrf" --data-urlencode "token=not-a-real-token")" || die "finance UI login POST: curl failed"
[ "$code" = 401 ] || die "finance UI: a wrong token should get 401, got $code"
ok "finance UI refuses a wrong token (HTTP 401)"
rm -f "$jar"
expect_status "finance API still answers behind the UI" 401 "http://finance.localhost:$port/v1/parties"
expect_status "finance public invoice link path stays with the API" 404 "http://finance.localhost:$port/public/invoices/not-a-link"

# 6. OPTIONAL (FINANCE_FLOW=1): the refusals of the invoice flow. finance is wired to can-core, so an
# invoice is approved only after a second person approved it in the admin UI (docs/local-compose.md);
# the full flow is the e2e suite's.
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
  [ "$code" = 403 ] || die "finance: an invoice can-core has not approved must be refused (403), got $code"
  ok "finance refuses to approve before can-core did (HTTP 403)"
fi

printf '\nSmoke test passed: %d checks.\n' "$pass"
