#!/usr/bin/env bash
# test-session.sh: run a local Copilot Studio test session for Northern Lights.
#
#   scripts/test-session.sh start          build, start server + Quick Tunnel, verify both
#   scripts/test-session.sh status         processes, public URL, tunnel health
#   scripts/test-session.sh requests [MIN] redacted /mcp requests from the last MIN minutes (default 10)
#   scripts/test-session.sh stop           stop the tunnel and the server
#
# Credentials come from the git-ignored deployments/.env and are never
# printed. Runtime files (logs, pids, the connector copy with the real host)
# go to deployments/copilot-studio/*.local.* and are git-ignored.
# ponytail: one local server + one Quick Tunnel; use a named tunnel for longer phases.
set -euo pipefail

cd "$(dirname "$0")/.."
PORT=8090
DIR=deployments/copilot-studio
ENV_FILE=deployments/.env
SERVER_LOG=$DIR/northern-lights.local.log
TUNNEL_LOG=$DIR/cloudflared.local.log
SERVER_PID=$DIR/northern-lights.local.pid
TUNNEL_PID=$DIR/cloudflared.local.pid
CONNECTOR=$DIR/northern-lights-connector.yaml
CONNECTOR_LOCAL=$DIR/northern-lights-connector.local.yaml

die() { echo "ERROR: $*" >&2; exit 1; }
running() { [ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null; }
tunnel_url() { grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' "$TUNNEL_LOG" 2>/dev/null | head -1; }
verify() { ( set -a; . "$ENV_FILE"; set +a; NL_URL="$1" NL_KEY="$NL_API_KEY" scripts/verify-mcp.sh ); }

stop_pid() { # stop_pid <pidfile> <name>
  if running "$1"; then
    kill -TERM "$(cat "$1")"
    for _ in $(seq 1 20); do running "$1" || break; sleep 0.5; done
    running "$1" && die "$2 did not stop (pid $(cat "$1"))"
    echo "stopped $2"
  else
    echo "$2 not running"
  fi
  rm -f "$1"
}

cmd_start() {
  [ -f "$ENV_FILE" ] || die "$ENV_FILE is missing (see README: NL_API_KEY, NL_WORKIVA_CLIENT_ID, NL_WORKIVA_CLIENT_SECRET)"
  command -v cloudflared >/dev/null || die "cloudflared is not installed"
  [ ! -e "$HOME/.cloudflared/config.yaml" ] || die "~/.cloudflared/config.yaml exists; Quick Tunnels refuse to start. Move it aside first."
  if ss -ltn | grep -q ":$PORT "; then
    die "port $PORT is already in use: $(ss -ltnp | grep ":$PORT " | grep -o 'users:.*'). Run '$0 stop' or stop that process."
  fi

  echo "== build"
  go build -ldflags "-X main.version=$(git describe --always --dirty)" -o bin/workiva-mcp ./cmd/workiva-mcp

  echo "== server on :$PORT (NL_DEBUG_HEADERS=1)"
  ( set -a; . "$ENV_FILE"; set +a
    export NL_LISTEN_ADDR=":$PORT" NL_DEBUG_HEADERS=1 NL_DISABLE_LOCALHOST_PROTECTION=true
    setsid nohup bin/workiva-mcp -config deployments/config.local.yaml >>"$SERVER_LOG" 2>&1 </dev/null &
    echo $! >"$SERVER_PID" )
  for _ in $(seq 1 30); do curl -sf -o /dev/null "http://localhost:$PORT/readyz" && break; sleep 0.5; done
  curl -sf -o /dev/null "http://localhost:$PORT/readyz" || die "server not ready; see $SERVER_LOG"
  grep 'configured key_sha256' "$SERVER_LOG" | tail -1 | sed 's/^.*nl-debug-headers /   /'
  verify "http://localhost:$PORT/mcp"

  echo "== Quick Tunnel"
  [ -s "$TUNNEL_LOG" ] && mv "$TUNNEL_LOG" "$TUNNEL_LOG.prev"
  setsid nohup cloudflared tunnel --url "http://localhost:$PORT" >"$TUNNEL_LOG" 2>&1 </dev/null &
  echo $! >"$TUNNEL_PID"
  for _ in $(seq 1 60); do grep -q 'Registered tunnel connection' "$TUNNEL_LOG" && [ -n "$(tunnel_url)" ] && break; sleep 1; done
  url=$(tunnel_url); [ -n "$url" ] || die "no tunnel URL after 60s; see $TUNNEL_LOG"
  for _ in $(seq 1 20); do [ "$(curl -s -m 10 -o /dev/null -w '%{http_code}' "$url/healthz")" = 200 ] && break; sleep 3; done
  sed "s/^host: YOUR_TUNNEL_HOST$/host: ${url#https://}/" "$CONNECTOR" >"$CONNECTOR_LOCAL"
  verify "$url/mcp"

  cat <<MSG

Ready.
  MCP URL:        $url/mcp
  Connector Host: ${url#https://}   (also written to $CONNECTOR_LOCAL)
If the host differs from the one in the Copilot Studio connector: update
Custom connectors > Edit > General > Host, save, then remove and re-add the
Northern Lights tool on the agent.
MSG
}

cmd_status() {
  running "$SERVER_PID" && echo "server:  running (pid $(cat "$SERVER_PID"))" || echo "server:  not running"
  running "$TUNNEL_PID" && echo "tunnel:  running (pid $(cat "$TUNNEL_PID"))" || echo "tunnel:  not running"
  url=$(tunnel_url)
  [ -n "$url" ] || { echo "url:     none"; return; }
  if ! running "$TUNNEL_PID"; then echo "url:     none (last was $url, dead once the tunnel stops)"; return; fi
  echo "url:     $url/mcp"
  echo "healthz: $(curl -s -m 10 -o /dev/null -w '%{http_code}' "$url/healthz") via tunnel"
  if grep -q 'Tunnel not found' "$TUNNEL_LOG"; then
    echo "WARNING: Cloudflare dropped this Quick Tunnel ('Tunnel not found'). Run stop, then start, and update the connector Host."
  fi
}

cmd_requests() {
  minutes=${1:-10}
  [ -f "$SERVER_LOG" ] || die "no server log at $SERVER_LOG"
  python3 - "$SERVER_LOG" "$minutes" <<'PY'
import re, sys, datetime
log, minutes = sys.argv[1], int(sys.argv[2])
cutoff = datetime.datetime.now() - datetime.timedelta(minutes=minutes)
pat = re.compile(r'^(\S+ \S+) nl-debug-headers request_id=\S+ method=\S+ mcp_method=(\S+) status=(\d+) error="([^"]*)" '
                 r'user_agent="([^"]*)" header_names=\[[^\]]*\] authorization=(?:absent|present scheme=(\S+) len=(\d+)) '
                 r'key_sha256=(\S+) nl-actor="([^"]*)"')
configured = "-"
rows = []
for line in open(log):
    if "configured key_sha256=" in line:
        configured = line.split("configured key_sha256=")[1].split()[0]
    m = pat.match(line)
    if not m:
        continue
    ts = datetime.datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S")
    if ts >= cutoff:
        rows.append(m)
print(f"configured key_sha256={configured}; last {minutes} min: {len(rows)} request(s)")
print(f"{'time':8}  {'mcp method':26} {'status':6} {'auth':12} {'key':8} {'match':5}  {'user agent':30} {'nl-actor':34} error")
for m in rows:
    auth = f"{m.group(6)}/{m.group(7)}" if m.group(6) else "absent"
    match = "yes" if m.group(8) == configured else ("-" if m.group(8) == "-" else "NO")
    print(f"{m.group(1)[11:]:8}  {m.group(2):26} {m.group(3):6} {auth:12} {m.group(8):8} {match:5}  "
          f"{m.group(5)[:30]:30} {(m.group(9) or '-')[:34]:34} {m.group(4)}")
PY
}

case "${1:-}" in
  start) cmd_start ;;
  stop) stop_pid "$TUNNEL_PID" cloudflared; stop_pid "$SERVER_PID" "Northern Lights" ;;
  status) cmd_status ;;
  requests) cmd_requests "${2:-10}" ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
