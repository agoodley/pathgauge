#!/usr/bin/env bash
# Start a local PathGauge server and agent with demo data, for dashboard
# development and manual testing. Everything lives under $DEV_DIR.
#
#   scripts/dev-stack.sh start   # build, start, seed demo runs
#   scripts/dev-stack.sh stop
#
# Dashboard: https://127.0.0.1:17443  (user admin, password admin-password-123)
set -euo pipefail
cd "$(dirname "$0")/.."
DIR=${DEV_DIR:-/tmp/pathgauge-dev}
PG=${PG:-$PWD/bin/pathgauge}
URL=https://127.0.0.1:17443
PASS=admin-password-123

stop() {
    for f in "$DIR"/*.pid; do
        [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null || true
        rm -f "$f"
    done
    echo "stopped"
}

api() { # api METHOD PATH [JSON]
    curl -sk -b "$DIR/cookies" -c "$DIR/cookies" -H 'Content-Type: application/json' -H 'X-PathGauge: 1' -X "$1" "$URL$2" ${3:+-d "$3"}
}

start() {
    stop >/dev/null
    rm -rf "$DIR"; mkdir -p "$DIR/server" "$DIR/agent"
    if [ "${SKIP_BUILD:-}" != 1 ]; then CGO_ENABLED=0 go build -o "$PG" ./cmd/pathgauge; fi
    cat > "$DIR/server.yaml" <<YAML
listen: "127.0.0.1:17443"
data_dir: $DIR/server
data_plane:
  listen: "127.0.0.1:17400"
log:
  level: info
YAML
    cat > "$DIR/agent.yaml" <<YAML
name: demo-agent
data_dir: $DIR/agent
policy_file: ""
data_plane:
  listen: "127.0.0.1:17401"
YAML
    echo "$PASS" > "$DIR/pw"
    "$PG" server init -c "$DIR/server.yaml" --admin admin --password-file "$DIR/pw" >/dev/null
    "$PG" server run -c "$DIR/server.yaml" > "$DIR/server.log" 2>&1 & echo $! > "$DIR/server.pid"
    for _ in $(seq 1 50); do curl -sk "$URL/api/health" >/dev/null && break; sleep 0.2; done
    TOKEN=$("$PG" server join-token -c "$DIR/server.yaml" | grep -o 'pgjoin_[a-z0-9_]*' | head -1)
    "$PG" agent join "$URL" --token "$TOKEN" --name demo-agent -c "$DIR/agent.yaml" >/dev/null
    "$PG" agent run -c "$DIR/agent.yaml" > "$DIR/agent.log" 2>&1 & echo $! > "$DIR/agent.pid"
    api POST /api/auth/login "{\"username\":\"admin\",\"password\":\"$PASS\"}" >/dev/null
    sleep 2
    AGENT=$(api GET /api/agents | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
    api PUT "/api/agents/$AGENT" '{"name":"demo-agent","site":"Lab","expected_down":0,"expected_up":0,"monitor_enabled":true,"monitor_interval":15}' >/dev/null
    if [ "${SEED:-1}" = 1 ]; then
        echo "seeding demo runs (about 2 minutes)…"
        for tests in '["info","latency","tcp-down","tcp-up","bufferbloat"]' '["info","latency","trace","tcp-down-1","tcp-up-1","tcp-down","tcp-up","udp-down","udp-up","pmtu","integrity","voip","sweep-down","streams-down"]'; do
            RID=$(api POST /api/runs "{\"agent_id\":\"$AGENT\",\"profile\":\"standard\",\"tests\":$tests,\"scale\":0.3,\"max_rate_mbps\":2000}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["run_id"])')
            while [ "$(api GET "/api/runs/$RID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = running ]; do sleep 2; done
        done
        api POST /api/alert-rules '{"name":"High latency","metric":"rtt_p95","op":">","threshold":50,"for_minutes":5,"channel":"log","target":"","enabled":true}' >/dev/null
        api POST /api/schedules "{\"agent_id\":\"$AGENT\",\"profile\":\"quick\",\"cron\":\"0 3 * * *\",\"enabled\":false,\"note\":\"nightly quick check\"}" >/dev/null
    fi
    echo "PathGauge dev stack running: $URL  (admin / $PASS)  agent $AGENT"
}

case "${1:-}" in
    start) start ;;
    stop) stop ;;
    *) echo "usage: $0 start|stop"; exit 2 ;;
esac
