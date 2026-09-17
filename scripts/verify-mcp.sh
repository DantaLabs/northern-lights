#!/usr/bin/env bash
# verify-mcp.sh: smoke-test a Northern Lights MCP endpoint over plain HTTP.
#
#   NL_URL=https://<host>/mcp NL_KEY=<api key> scripts/verify-mcp.sh
#
# Sends initialize, notifications/initialized (keeping Mcp-Session-Id when
# the server returns one) and tools/list, checks for exactly 7 tools and
# application/json responses, then repeats tools/list with a malformed key
# and expects 401. Exits non-zero on the first failed check. NL_KEY is
# passed to curl through a private header file and is never printed.
set -euo pipefail

: "${NL_URL:?set NL_URL to the /mcp endpoint, e.g. http://localhost:8090/mcp}"
: "${NL_KEY:?set NL_KEY to the Northern Lights API key}"
command -v python3 >/dev/null || { echo "FAIL: python3 is required to parse JSON" >&2; exit 2; }

umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf 'Authorization: Bearer %s\n' "$NL_KEY" > "$work/auth"
printf 'Authorization: Bearer malformed-key\n' > "$work/badauth"
session=""
failures=0

# post <auth header file> <json body>; sets status, ctype, body and session.
post() {
  local hdr=$1 payload=$2
  local extra=()
  [ -n "$session" ] && extra=(-H "Mcp-Session-Id: $session")
  status=$(curl -sS -o "$work/body" -D "$work/headers" -w '%{http_code}' \
    -X POST "$NL_URL" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -H "@$hdr" "${extra[@]}" \
    --data "$payload")
  ctype=$(awk 'tolower($1)=="content-type:" {print $2}' "$work/headers" | tr -d '\r' | tail -1)
  body=$(cat "$work/body")
  local sid
  sid=$(awk 'tolower($1)=="mcp-session-id:" {print $2}' "$work/headers" | tr -d '\r' | tail -1)
  [ -n "$sid" ] && session=$sid
}

check() { # check <description> <condition-exit-code>
  if [ "$2" -eq 0 ]; then echo "PASS: $1"; else echo "FAIL: $1"; failures=$((failures + 1)); fi
}

post "$work/auth" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"verify-mcp","version":"1"}}}'
check "initialize returns 200 (got $status)" "$([ "$status" = 200 ]; echo $?)"
check "initialize answers application/json, not SSE (got ${ctype:-none})" "$([[ "$ctype" == application/json* ]]; echo $?)"
[ -n "$session" ] && echo "INFO: server issued Mcp-Session-Id (kept for the session)"

post "$work/auth" '{"jsonrpc":"2.0","method":"notifications/initialized"}'
check "notifications/initialized returns 202 (got $status)" "$([ "$status" = 202 ]; echo $?)"

post "$work/auth" '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
check "tools/list returns 200 (got $status)" "$([ "$status" = 200 ]; echo $?)"
check "tools/list answers application/json (got ${ctype:-none})" "$([[ "$ctype" == application/json* ]]; echo $?)"
count=$(printf '%s' "$body" | python3 -c 'import json,sys
try:
    print(len(json.load(sys.stdin)["result"]["tools"]))
except Exception:
    print(-1)')
check "tools/list has exactly 7 tools (got $count)" "$([ "$count" = 7 ]; echo $?)"

post "$work/badauth" '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}'
check "malformed key is rejected with 401 (got $status)" "$([ "$status" = 401 ]; echo $?)"

if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed against $NL_URL" >&2
  exit 1
fi
echo "all checks passed against $NL_URL"
