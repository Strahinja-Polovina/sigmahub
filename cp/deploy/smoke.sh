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
# provisions a uniquely-named throwaway org each time and leaves the ORG behind
# (the CP has no org teardown API in v1; delete the smoke orgs out-of-band if
# desired) — but NOT the credentials, see below.
#
# Credential custody (SIGMA-267). This script mints live Org Admin tokens and it
# runs often: staging.md tells the operator to run it after every bring-up, and
# the deploy workflow runs it on every push to main, on a box that also runs
# design-partner workloads. So the two ways it used to hand out org-admin
# authority are both closed here:
#
#   * response bodies go to a mktemp file (0600, under TMPDIR) that an EXIT trap
#     removes, not to a fixed, world-guessable /tmp path created with the
#     process umask and left there. The provision response is one of those
#     bodies, and it carries an Org Admin token in plaintext.
#   * every token minted during the run is revoked on the way out, whether the
#     run passed, failed or was interrupted. What is left behind is an empty
#     org, not authority over one.
set -euo pipefail

CP_URL="${CP_URL:-http://localhost:8080}"
PROV="${CP_PROVISION_TOKEN:?set CP_PROVISION_TOKEN}"
ORG="${SMOKE_ORG:-smoke-$(date +%s)}"
PASS=0
FAIL=0

# 0600 by construction, and under TMPDIR rather than at a name anyone can guess.
BODY_FILE="$(mktemp "${TMPDIR:-/tmp}/smoke.body.XXXXXXXX")"
# "org|tokenId|token" per minted credential, revoked by the EXIT trap.
MINTED=()

# Runs on every exit path, including the failed-assertion one and Ctrl-C, which
# are exactly the paths on which a token would otherwise survive. Nothing in
# here may fail the script: the trap inherits `set -e`, so an unreachable
# control plane during cleanup would REPLACE the script's real exit status with
# curl's — turning a passing smoke check into a red deploy. Hence `|| true`.
cleanup() {
  rm -f "$BODY_FILE" || true
  local entry org id tok
  for entry in ${MINTED[@]+"${MINTED[@]}"}; do
    IFS='|' read -r org id tok <<<"$entry"
    [ -n "$id" ] && [ -n "$tok" ] || continue
    curl -sS -o /dev/null -X DELETE \
      -H "Authorization: Bearer $tok" \
      "$CP_URL/v1/orgs/$org/service-tokens/$id" || true
  done
}
trap cleanup EXIT

say()  { printf '\033[36m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓ %s\033[0m\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31m✗ %s\033[0m\n' "$*"; FAIL=$((FAIL+1)); }

# req METHOD PATH [AUTH] [BODY] → sets $BODY (response) and $CODE (status).
req() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(-sS -o "$BODY_FILE" -w '%{http_code}' -X "$method" "$CP_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  CODE="$(curl "${args[@]}")"
  BODY="$(cat "$BODY_FILE")"
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
# Remembered before anything else can fail, so the revoke happens even if the
# very next assertion aborts the run.
MINTED+=("$ORG|$(jget tokenId)|$TOKEN")
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
MINTED+=("$ORG2|$(jget tokenId)|$(jget token)")
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
