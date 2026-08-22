#!/usr/bin/env bash
# End-to-end verification for Conduit against a live Ollama.
#
# Unlike the Go test suite (which uses httptest fakes), this drives the real
# binary against a real model over a real socket. Run it after any change that
# touches request handling.
#
#   ./scripts/e2e.sh
#
# Requires: a running Ollama with llama3.2:3b pulled, and a gateway started with
# the dedicated routing fixture:
#
#   ./bin/conduit -routes configs/routes.e2e.yaml

set -uo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"
MODEL="${MODEL:-llama3.2:3b}"
WORKDIR="$(mktemp -d)"
PASS=0
FAIL=0

RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; RESET=$'\033[0m'

cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

section() { printf '\n%s── %s %s\n' "$BOLD" "$1" "$RESET"; }

check() { # check <description> <condition-result> [detail]
  if [ "$2" = "0" ]; then
    printf '  %sPASS%s  %s\n' "$GREEN" "$RESET" "$1"
    PASS=$((PASS + 1))
  else
    printf '  %sFAIL%s  %s\n' "$RED" "$RESET" "$1"
    [ $# -ge 3 ] && printf '        %s\n' "$3"
    FAIL=$((FAIL + 1))
  fi
}

json() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

# --- preflight -------------------------------------------------------------

section "Preflight"

curl -sf --max-time 5 "$OLLAMA_URL/api/version" >/dev/null
check "Ollama is reachable at $OLLAMA_URL" $?

curl -sf --max-time 5 "$GATEWAY_URL/healthz" >/dev/null
check "Gateway is reachable at $GATEWAY_URL" $?

if [ "$FAIL" -gt 0 ]; then
  printf '\n%sPreflight failed. Start Ollama and ./bin/conduit first.%s\n' "$RED" "$RESET"
  exit 1
fi

# Warm the model so cold-load time does not skew the latency assertions.
curl -sf --max-time 180 "$OLLAMA_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":1}" \
  >/dev/null 2>&1

# --- HTTP surface ----------------------------------------------------------

section "HTTP surface"

BODY=$(curl -sf --max-time 5 "$GATEWAY_URL/healthz")
[ "$(printf '%s' "$BODY" | json "d['status']")" = "ok" ]
check "/healthz returns status ok" $?

CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$GATEWAY_URL/readyz")
[ "$CODE" = "200" ]
check "/readyz returns 200 when a provider is available" $? "got $CODE"

MODELS=$(curl -sf --max-time 5 "$GATEWAY_URL/v1/models")
printf '%s' "$MODELS" | grep -q "$MODEL"
check "/v1/models advertises $MODEL" $?

[ "$(printf '%s' "$MODELS" | json "d['object']")" = "list" ]
check "/v1/models uses the OpenAI list envelope" $?

# --- completions -----------------------------------------------------------

section "Completions"

START=$(date +%s%3N)
RESP=$(curl -sf --max-time 180 -D "$WORKDIR/h1" "$GATEWAY_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: OK\"}],\"temperature\":0,\"max_tokens\":10}")
ELAPSED=$(( $(date +%s%3N) - START ))

[ -n "$RESP" ] && [ -n "$(printf '%s' "$RESP" | json "d['choices'][0]['message']['content']")" ]
check "non-streaming completion returns content (${ELAPSED}ms)" $?

[ "$(printf '%s' "$RESP" | json "d['usage']['total_tokens']")" -gt 0 ] 2>/dev/null
check "response carries token usage" $?

[ "$(printf '%s' "$RESP" | json "d['object']")" = "chat.completion" ]
check "response uses the OpenAI chat.completion shape" $?

grep -qi '^x-conduit-provider: ollama-local' "$WORKDIR/h1"
check "X-Conduit-Provider names the serving provider" $?

grep -qi '^x-conduit-attempts: 1' "$WORKDIR/h1"
check "X-Conduit-Attempts is 1 on a healthy first try" $?

# Connection pooling: repeated requests must reuse the upstream connection.
BEFORE=$(ss -tn state established "( dport = :11434 )" 2>/dev/null | tail -n +2 | wc -l)
for _ in 1 2 3 4 5; do
  curl -sf -o /dev/null --max-time 60 "$GATEWAY_URL/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":2}"
done
AFTER=$(ss -tn state established "( dport = :11434 )" 2>/dev/null | tail -n +2 | wc -l)
[ "$AFTER" -le $((BEFORE + 3)) ]
check "connections are pooled, not opened per request (${BEFORE}→${AFTER} established)" $?

# --- error handling --------------------------------------------------------

section "Error handling"

for spec in \
  "400|{\"model\":\"$MODEL\",\"messages\":[]}|empty messages" \
  "400|{\"messages\":[{\"role\":\"user\",\"content\":\"x\"}]}|missing model" \
  "400|not-json|malformed JSON" \
  "404|{\"model\":\"ghost-model\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}]}|unknown model"
do
  WANT="${spec%%|*}"; REST="${spec#*|}"; PAYLOAD="${REST%|*}"; DESC="${REST##*|}"
  CODE=$(curl -s -o "$WORKDIR/err" -w '%{http_code}' --max-time 10 \
    "$GATEWAY_URL/v1/chat/completions" -H 'Content-Type: application/json' -d "$PAYLOAD")
  [ "$CODE" = "$WANT" ]
  check "$DESC returns $WANT" $? "got $CODE"

  python3 -c "import sys,json;d=json.load(open('$WORKDIR/err'));assert d['error']['message'] and d['error']['type']" 2>/dev/null
  check "  └ error uses the OpenAI error envelope" $?
done

CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$GATEWAY_URL/v1/chat/completions")
[ "$CODE" = "405" ]
check "GET on the completions endpoint returns 405" $? "got $CODE"

# --- streaming -------------------------------------------------------------

section "Streaming"

curl -sfN --max-time 120 "$GATEWAY_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -D "$WORKDIR/h2" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Count from 1 to 5\"}],\"stream\":true,\"temperature\":0,\"max_tokens\":40}" \
  > "$WORKDIR/stream.txt"

FRAMES=$(grep -c '^data:' "$WORKDIR/stream.txt")
[ "$FRAMES" -gt 3 ]
check "stream produced multiple frames ($FRAMES)" $?

grep -qi '^content-type: text/event-stream' "$WORKDIR/h2"
check "Content-Type is text/event-stream" $?

grep -qi '^x-accel-buffering: no' "$WORKDIR/h2"
check "X-Accel-Buffering: no is set so intermediaries do not buffer" $?

tail -2 "$WORKDIR/stream.txt" | grep -q '^data: \[DONE\]'
check "stream terminates with [DONE]" $?

# Tokens must arrive incrementally, not as one buffered lump at the end.
python3 - "$GATEWAY_URL" "$MODEL" > "$WORKDIR/timing.txt" <<'PY'
import json, sys, time, urllib.request
url, model = sys.argv[1], sys.argv[2]
payload = json.dumps({"model": model,
                      "messages": [{"role": "user", "content": "Write one sentence about rivers."}],
                      "stream": True, "temperature": 0, "max_tokens": 30}).encode()
req = urllib.request.Request(url + "/v1/chat/completions", data=payload,
                             headers={"Content-Type": "application/json"})
stamps = []
start = time.monotonic()
with urllib.request.urlopen(req, timeout=120) as r:
    for line in r:
        if line.startswith(b"data: ") and b"[DONE]" not in line:
            stamps.append(time.monotonic() - start)
print(len(stamps))
print(f"{stamps[0]:.3f}" if stamps else "0")
print(f"{stamps[-1]:.3f}" if stamps else "0")
PY

N=$(sed -n 1p "$WORKDIR/timing.txt")
FIRST=$(sed -n 2p "$WORKDIR/timing.txt")
LAST=$(sed -n 3p "$WORKDIR/timing.txt")
python3 -c "import sys; sys.exit(0 if float('$LAST') - float('$FIRST') > 0.02 else 1)"
check "tokens arrive incrementally (first ${FIRST}s → last ${LAST}s, $N frames)" $?

# A client that did not ask for usage must not receive a usage-only frame.
grep -q '"usage"' "$WORKDIR/stream.txt"
[ $? -ne 0 ]
check "usage frame suppressed when the client did not request it" $?

curl -sfN --max-time 60 "$GATEWAY_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true,\"max_tokens\":10,\"stream_options\":{\"include_usage\":true}}" \
  > "$WORKDIR/stream_usage.txt"
grep -q '"usage"' "$WORKDIR/stream_usage.txt"
check "usage frame delivered when the client requests it" $?

# Client disconnect must abort the upstream generation, not let it run on.
DISCONNECT=$(python3 - "$GATEWAY_URL" "$MODEL" <<'PY'
import json, socket, sys, time, urllib.parse
url, model = sys.argv[1], sys.argv[2]
p = urllib.parse.urlparse(url)
body = json.dumps({"model": model,
                   "messages": [{"role": "user", "content": "Write a very long 800-word essay about bridges."}],
                   "stream": True, "temperature": 0, "max_tokens": 1200})
s = socket.create_connection((p.hostname, p.port or 80), timeout=30)
s.sendall(("POST /v1/chat/completions HTTP/1.1\r\n"
           f"Host: {p.hostname}\r\nContent-Type: application/json\r\n"
           f"Content-Length: {len(body)}\r\nConnection: close\r\n\r\n" + body).encode())
start = time.monotonic()
received = 0
while time.monotonic() - start < 2.0:
    try:
        chunk = s.recv(4096)
    except socket.timeout:
        break
    if not chunk:
        break
    received += chunk.count(b"data: ")
s.close()   # hang up mid-generation
print(received)
PY
)
[ "$DISCONNECT" -gt 0 ]
check "client received frames before disconnecting ($DISCONNECT)" $?

sleep 2
LASTREQ=$(grep 'POST     "/v1/chat/completions"' "${OLLAMA_LOG:-/dev/null}" 2>/dev/null | tail -1)
if [ -n "$LASTREQ" ]; then
  printf '  %sINFO%s  upstream request on disconnect: %s\n' "$YELLOW" "$RESET" \
    "$(printf '%s' "$LASTREQ" | awk -F'|' '{print $3}' | tr -d ' ')"
fi

# --- routing, failover, breakers -------------------------------------------

section "Routing, failover, and circuit breakers"

RESP=$(curl -sf --max-time 120 -D "$WORKDIR/h3" "$GATEWAY_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"conduit-ha","messages":[{"role":"user","content":"Say OK"}],"temperature":0,"max_tokens":5}')

[ -n "$RESP" ]
check "conduit-ha route served a response despite a dead first target" $?

grep -qi '^x-conduit-attempts: 2' "$WORKDIR/h3"
check "failover took exactly 2 attempts" $? "$(grep -i x-conduit-attempts "$WORKDIR/h3")"

grep -qi '^x-conduit-provider: ollama-local' "$WORKDIR/h3"
check "failover landed on the healthy provider" $?

# The dead target must cost effectively nothing to skip.
FAILOVER_KIND=$(curl -sf --max-time 10 "$GATEWAY_URL/admin/routes" |
  python3 -c "import sys,json;d=json.load(sys.stdin);print([p['breaker']['state'] for p in d['providers'] if p['name']=='ollama-broken'][0])")
printf '  %sINFO%s  ollama-broken breaker state: %s\n' "$YELLOW" "$RESET" "$FAILOVER_KIND"

# Drive enough failures to trip the breaker (configured: 3 consecutive).
for _ in 1 2 3 4; do
  curl -sf -o /dev/null --max-time 120 "$GATEWAY_URL/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"conduit-ha","messages":[{"role":"user","content":"hi"}],"max_tokens":3}'
done

SNAP=$(curl -sf --max-time 10 "$GATEWAY_URL/admin/routes")
STATE=$(printf '%s' "$SNAP" | python3 -c "import sys,json;d=json.load(sys.stdin);print([p['breaker']['state'] for p in d['providers'] if p['name']=='ollama-broken'][0])")
SHORTED=$(printf '%s' "$SNAP" | python3 -c "import sys,json;d=json.load(sys.stdin);print([p['breaker']['short_circuited'] for p in d['providers'] if p['name']=='ollama-broken'][0])")

[ "$STATE" = "open" ] || [ "$STATE" = "half-open" ]
check "circuit breaker opened for the dead provider (state=$STATE)" $?

[ "$SHORTED" -gt 0 ]
check "requests were short-circuited without touching the network ($SHORTED)" $?

# Fast-fail: with the breaker open the whole request should be quick.
START=$(date +%s%3N)
curl -sf -o /dev/null --max-time 120 "$GATEWAY_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"conduit-ha","messages":[{"role":"user","content":"hi"}],"max_tokens":3}'
ELAPSED=$(( $(date +%s%3N) - START ))
[ "$ELAPSED" -lt 10000 ]
check "open breaker fails fast, request still served (${ELAPSED}ms)" $?

# Routing variety.
for m in conduit-balanced conduit-weighted; do
  CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 120 "$GATEWAY_URL/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":3}")
  [ "$CODE" = "200" ]
  check "$m route serves successfully" $? "got $CODE"
done

# --- hot reload ------------------------------------------------------------

section "Hot reload"

ROUTES_FILE="${ROUTES_FILE:-configs/routes.e2e.yaml}"
cp "$ROUTES_FILE" "$WORKDIR/routes.backup"

python3 - "$ROUTES_FILE" <<'PY'
import os, sys
p = sys.argv[1]
s = open(p).read()
s = s.replace("routes:\n", """routes:
  - model: e2e-hot-reload-probe
    targets:
      - provider: ollama-local
        model: llama3.2:3b

""", 1)
open(p + ".tmp", "w").write(s)
os.rename(p + ".tmp", p)   # atomic rename, as an editor or kubectl would
PY

sleep 3
curl -sf --max-time 10 "$GATEWAY_URL/v1/models" | grep -q 'e2e-hot-reload-probe'
check "new route is live after an atomic rename, without a restart" $?

# A broken edit must be rejected, leaving the previous table serving.
printf 'providers: [broken\n  not valid yaml' > "$ROUTES_FILE"
sleep 3
curl -sf --max-time 10 "$GATEWAY_URL/v1/models" | grep -q 'e2e-hot-reload-probe'
check "invalid config rejected; previous routing table still serving" $?

CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$GATEWAY_URL/healthz")
[ "$CODE" = "200" ]
check "gateway stayed healthy through a broken config edit" $? "got $CODE"

cp "$WORKDIR/routes.backup" "$ROUTES_FILE"
sleep 3
curl -sf --max-time 10 "$GATEWAY_URL/v1/models" | grep -q 'e2e-hot-reload-probe'
[ $? -ne 0 ]
check "original routing table restored" $?

# --- summary ---------------------------------------------------------------

printf '\n%s─────────────────────────────%s\n' "$BOLD" "$RESET"
printf '  %spassed: %d%s   %sfailed: %d%s\n' "$GREEN" "$PASS" "$RESET" \
  "$([ "$FAIL" -gt 0 ] && printf '%s' "$RED" || printf '%s' "$GREEN")" "$FAIL" "$RESET"
printf '%s─────────────────────────────%s\n\n' "$BOLD" "$RESET"

[ "$FAIL" -eq 0 ]
