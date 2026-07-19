#!/usr/bin/env bash
# Full-stack smoke / acceptance check for a SigmaHub control plane (P2 beta
# launch, SIGMA-53/54). Exercises the real HTTP surface end to end against a
# RUNNING stack — no mocks — so a staging bring-up is verified with one command:
#
#   CP_URL=https://cp.staging.example \
#   CP_PROVISION_TOKEN=... \
#   ./smoke.sh
#
# Exit 0 = the control plane is healthy and the core provisioning path works
# (org → project → environment → agent-enrollment token → reads). Exit 1 on the
# first failed assertion, printing the request that failed. Safe to re-run: it
# provisions a uniquely-named throwaway org each time and leaves it (the CP has
# no org teardown API in v1; delete the smoke org out-of-band if desired).
set -euo pipefail

CP_URL="${CP_URL:-http://localhost:8080}"
PROV="${CP_PROVISION_TOKEN:?set CP_PROVISION_TOKEN}"
ORG="${SMOKE_ORG:-smoke-$(date +%s)}"
PASS=0
FAIL=0

say()  { printf '\033[36m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓ %s\033[0m\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31m✗ %s\033[0m\n' "$*"; FAIL=$((FAIL+1)); }

# req METHOD PATH [AUTH] [BODY] → sets $BODY (response) and $CODE (status).
req() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(-sS -o /tmp/smoke.body -w '%{http_code}' -X "$method" "$CP_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  CODE="$(curl "${args[@]}")"
  BODY="$(cat /tmp/smoke.body)"
}

# expect EXPECTED_CODE LABEL
expect() {
  if [ "$CODE" = "$1" ]; then ok "$2 ($CODE)"; else bad "$2 — got $CODE, want $1: $BODY"; fi
}

# jget KEY → prints the top-level JSON string value for KEY from $BODY.
jget() { printf '%s' "$BODY" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -1; }

say "1. Liveness + readiness"
req GET /healthz;  expect 200 "GET /healthz"
req GET /readyz;   expect 200 "GET /readyz (DB reachable)"

say "2. Provision a throwaway org ($ORG)"
req POST /v1/orgs "$PROV" "{\"orgId\":\"$ORG\",\"name\":\"smoke $ORG\"}"
expect 201 "POST /v1/orgs"
TOKEN="$(jget token)"
[ -n "$TOKEN" ] && ok "org-admin token minted" || bad "no token in provision response: $BODY"

say "3. Unauthenticated + wrong-token are rejected"
req GET "/v1/orgs/$ORG/projects";                 expect 401 "no token → 401"
req GET "/v1/orgs/$ORG/projects" "not-a-token";   expect 401 "bad token → 401"

say "4. Project + environment (org-admin token)"
req POST "/v1/orgs/$ORG/projects" "$TOKEN" '{"name":"smoke-app"}'
expect 201 "POST project"
PROJ="$(jget id)"
[ -n "$PROJ" ] && ok "project id $PROJ" || bad "no project id: $BODY"
req POST "/v1/orgs/$ORG/projects/$PROJ/environments" "$TOKEN" '{"name":"prod","production":true}'
expect 201 "POST environment"
ENV="$(jget id)"
[ -n "$ENV" ] && ok "environment id $ENV" || bad "no env id: $BODY"

say "5. Agent enrollment token (SSH onboarding entry)"
req POST "/v1/orgs/$ORG/bootstrap-tokens" "$TOKEN" '{"name":"smoke-host","type":"general"}'
expect 201 "POST bootstrap-token"
[ -n "$(jget token)" ] && ok "bootstrap token issued" || bad "no bootstrap token: $BODY"

say "6. Read models reflect the writes"
req GET "/v1/orgs/$ORG/projects" "$TOKEN";                    expect 200 "GET projects"
req GET "/v1/orgs/$ORG/projects/$PROJ/environments" "$TOKEN"; expect 200 "GET environments"
req GET "/v1/orgs/$ORG/servers" "$TOKEN";                     expect 200 "GET servers (empty until an agent enrolls)"

say "7. Tenant isolation (SIGMA-54): a token can't reach another org"
ORG2="${ORG}-b"
req POST /v1/orgs "$PROV" "{\"orgId\":\"$ORG2\",\"name\":\"smoke $ORG2\"}"
expect 201 "provision second org"
req GET "/v1/orgs/$ORG2/projects" "$TOKEN"
expect 403 "org-A token → org-B read is 403 (cross-tenant denied)"

echo
if [ "$FAIL" -eq 0 ]; then
  printf '\033[32mSMOKE PASS — %d checks, control plane healthy.\033[0m\n' "$PASS"
  exit 0
else
  printf '\033[31mSMOKE FAIL — %d passed, %d failed.\033[0m\n' "$PASS" "$FAIL"
  exit 1
fi
