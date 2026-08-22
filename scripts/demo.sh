#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${CONDUIT_URL:-http://localhost:8080}"
OWNER_EMAIL="${CONDUIT_OWNER_EMAIL:-owner@conduit.local}"
OWNER_NAME="${CONDUIT_OWNER_NAME:-Local Owner}"
OWNER_PASSWORD="${CONDUIT_OWNER_PASSWORD:-ConduitLocal!2026}"
WORK_DIR="$(mktemp -d /tmp/conduit-demo.XXXXXX)"
trap 'rm -rf "$WORK_DIR"' EXIT

note() { printf '  %-28s %s\n' "$1" "$2"; }

docker compose exec -T gateway /app/conduit admin bootstrap \
  --email "$OWNER_EMAIL" --name "$OWNER_NAME" --password "$OWNER_PASSWORD" \
  --postgres-dsn 'postgres://conduit:conduit@postgres:5432/conduit?sslmode=disable' >/dev/null 2>&1 || true

login="$(curl -fsS -c "$WORK_DIR/cookies" "$BASE_URL/control/v1/session/login" \
  -H 'Content-Type: application/json' -d "{\"email\":\"$OWNER_EMAIL\",\"password\":\"$OWNER_PASSWORD\"}")"
csrf="$(jq -r .csrf_token <<<"$login")"
slug="demo-$(date +%s)"

post() { curl -fsS -b "$WORK_DIR/cookies" -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" "$@"; }
project="$(post "$BASE_URL/control/v1/projects" -d "{\"slug\":\"$slug\",\"name\":\"Conduit Demo\",\"description\":\"A deterministic application integration story\"}")"
project_id="$(jq -r .id <<<"$project")"
post -X PUT "$BASE_URL/control/v1/projects/$project_id" -d '{"name":"Conduit Demo Project","description":"An application integration story with explicit model policy"}' >/dev/null

provider_a="$(post "$BASE_URL/control/v1/projects/$project_id/providers" -d '{"name":"mock-primary","type":"openai_compatible","base_url":"http://mock-provider:19100","timeout_seconds":10,"breaker_policy":{}}')"
provider_b="$(post "$BASE_URL/control/v1/projects/$project_id/providers" -d '{"name":"mock-fallback","type":"openai_compatible","base_url":"http://mock-provider:19100","timeout_seconds":10,"breaker_policy":{}}')"
a_id="$(jq -r .id <<<"$provider_a")"; b_id="$(jq -r .id <<<"$provider_b")"
provider_check="$(post -X POST "$BASE_URL/control/v1/projects/$project_id/providers/$a_id/test" -d '{}')"
if [[ "$(jq -r .ok <<<"$provider_check")" != true ]]; then
  printf 'mock provider connection test failed: %s\n' "$(jq -r .message <<<"$provider_check")" >&2
  exit 1
fi
rotated_provider="$(post -X PUT "$BASE_URL/control/v1/projects/$project_id/providers/$a_id/credential" -d '{"credential":"demo-rotated-secret"}')"
rotated_fingerprint="$(jq -r .credential_fingerprint <<<"$rotated_provider")"

profile_body="$(jq -nc --arg a "$a_id" --arg b "$b_id" '{name:"fast",description:"Stable low-latency profile",kind:"generation",strategy:"priority",enabled:true,max_attempts:2,targets:[{provider_connection_id:$a,upstream_model_id:"mock-model",priority:1,weight:1,input_price_per_million:0.1,output_price_per_million:0.2},{provider_connection_id:$b,upstream_model_id:"mock-model",priority:2,weight:1,input_price_per_million:0.1,output_price_per_million:0.2}]}')"
profile="$(post "$BASE_URL/control/v1/projects/$project_id/profiles" -d "$profile_body")"
profile_id="$(jq -r .id <<<"$profile")"
embedding_body="$(jq -nc --arg a "$a_id" '{name:"search",description:"Vectors for search and retrieval",kind:"embedding",strategy:"priority",enabled:true,max_attempts:1,targets:[{provider_connection_id:$a,upstream_model_id:"mock-embedding",priority:1,weight:1,input_price_per_million:0.02,output_price_per_million:0}]}')"
post "$BASE_URL/control/v1/projects/$project_id/profiles" -d "$embedding_body" >/dev/null

post -X PUT "$BASE_URL/control/v1/projects/$project_id/policy" -d "{\"project_id\":\"$project_id\",\"requests_per_minute\":120,\"tokens_per_minute\":100000,\"end_user_requests_per_minute\":30,\"monthly_budget_usd\":10,\"budget_warning_percent\":80,\"require_end_user\":true,\"cache_mode\":\"exact\",\"cache_ttl_seconds\":3600,\"semantic_threshold\":0.95,\"maximum_cacheable_temperature\":0.3,\"analytics_retention_days\":30}" >/dev/null

key_row="$(post "$BASE_URL/control/v1/projects/$project_id/keys" -d '{"label":"demo-application","environment":"test","allowed_profiles":["fast","search"]}')"
api_key="$(jq -r .plaintext <<<"$key_row")"; key_id="$(jq -r .id <<<"$key_row")"

call_gateway() {
  local prompt="$1" headers="$2"
  curl -fsS -D "$headers" "$BASE_URL/v1/chat/completions" -H "Authorization: Bearer $api_key" \
    -H 'X-Conduit-End-User: demo-user-42' -H 'Content-Type: application/json' \
    -d "{\"model\":\"fast\",\"messages\":[{\"role\":\"user\",\"content\":\"$prompt\"}],\"temperature\":0}"
}

call_gateway 'first routing check' "$WORK_DIR/first.headers" >/dev/null
first_provider="$(tr -d '\r' <"$WORK_DIR/first.headers" | awk -F': ' 'tolower($1)=="x-conduit-provider"{print $2}')"

curl -fsS "$BASE_URL/v1/embeddings" -H "Authorization: Bearer $api_key" \
  -H 'X-Conduit-End-User: demo-user-42' -H 'Content-Type: application/json' \
  -d '{"model":"search","input":"Conduit routes models for application teams"}' | jq -e '.data[0].embedding | length > 0' >/dev/null

curl -fsS "$BASE_URL/v1/responses" -H "Authorization: Bearer $api_key" \
  -H 'X-Conduit-End-User: demo-user-42' -H 'Content-Type: application/json' \
  -d '{"model":"fast","instructions":"Answer briefly","input":"What does Conduit do?"}' | jq -e '.object == "response" and .status == "completed"' >/dev/null

updated="$(jq -nc --arg a "$a_id" --arg b "$b_id" '{name:"fast",description:"Same profile with a new primary",kind:"generation",strategy:"priority",enabled:true,max_attempts:2,targets:[{provider_connection_id:$b,upstream_model_id:"mock-model",priority:1,weight:1,input_price_per_million:0.1,output_price_per_million:0.2},{provider_connection_id:$a,upstream_model_id:"mock-model",priority:2,weight:1,input_price_per_million:0.1,output_price_per_million:0.2}]}')"
post -X PUT "$BASE_URL/control/v1/projects/$project_id/profiles/$profile_id" -d "$updated" >/dev/null
call_gateway 'second routing check' "$WORK_DIR/second.headers" >/dev/null
second_provider="$(tr -d '\r' <"$WORK_DIR/second.headers" | awk -F': ' 'tolower($1)=="x-conduit-provider"{print $2}')"

usage_rows=0
for _ in $(seq 1 10); do
  recent_usage="$(curl -fsS -b "$WORK_DIR/cookies" "$BASE_URL/control/v1/projects/$project_id/usage/requests?limit=10")"
  usage_rows="$(jq length <<<"$recent_usage")"
  [[ "$usage_rows" -ge 4 ]] && break
  sleep 1
done
user_usage="$(post -X POST "$BASE_URL/control/v1/projects/$project_id/usage/end-users/search?limit=10" -d '{"end_user":"demo-user-42"}')"
user_usage_rows="$(jq length <<<"$user_usage")"
if [[ "$usage_rows" -lt 4 || "$user_usage_rows" -lt 4 ]]; then
  printf 'project or end-user usage did not reach ClickHouse\n' >&2
  exit 1
fi

archived="$(post -X POST "$BASE_URL/control/v1/projects/$project_id/archive" -d '{}')"
archived_key_status="$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/v1/models" -H "Authorization: Bearer $api_key")"
post -X POST "$BASE_URL/control/v1/projects/$project_id/keys/$key_id/revoke" >/dev/null
revoked_status="$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/v1/models" -H "Authorization: Bearer $api_key")"
audit_count="$(curl -fsS -b "$WORK_DIR/cookies" "$BASE_URL/control/v1/projects/$project_id/audit" | jq length)"

printf '\nConduit demo completed\n\n'
note 'Project' "$slug"
note 'Stable profile' 'fast'
note 'Embedding profile' 'search'
note 'Supported APIs' 'chat completions, responses, embeddings'
note 'Application key' "$(jq -r .prefix <<<"$key_row")… (shown once, then revoked)"
note 'First provider' "$first_provider"
note 'After profile change' "$second_provider"
note 'Provider check' "HTTP $(jq -r .status_code <<<"$provider_check") in $(jq -r .latency_ms <<<"$provider_check") ms"
note 'Rotated credential' "fingerprint $rotated_fingerprint"
note 'Attributed usage' "$user_usage_rows rows for demo-user-42"
note 'Archived project key' "HTTP $archived_key_status ($(jq -r .status <<<"$archived"))"
note 'Revoked key response' "HTTP $revoked_status"
note 'Audit events' "$audit_count"
printf '\nDashboard: http://localhost:3000/login\nLogin: %s / %s (development only)\n' "$OWNER_EMAIL" "$OWNER_PASSWORD"
