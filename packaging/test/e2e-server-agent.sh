#!/usr/bin/env bash
# End-to-end test of the packaged server and agent under real systemd:
# a Rocky 9 server and a Rocky 8 agent (by default) in Docker containers on
# a private network. Installs the RPM, runs `server init` and `agent join`
# through the CLI, starts both services, runs tests through the REST API and
# checks the results. Run from the repo root after `make snapshot`.
#
#   packaging/test/e2e-server-agent.sh [server-image] [agent-image]
set -euo pipefail
cd "$(dirname "$0")/../.."

SERVER_IMG=${1:-pg-sd-rocky9}
AGENT_IMG=${2:-pg-sd-rocky8}
NET=pg-e2e
RPM=$(find dist -name '*.x86_64.rpm' | head -1)
[ -n "$RPM" ] || { echo "no RPM in dist/ (run make snapshot)"; exit 1; }
PASS='e2e-admin-password'
fail() { echo "FAIL: $*" >&2; docker exec pg-e2e-server journalctl -u pathgauge-server --no-pager -n 30 >&2 || true; docker exec pg-e2e-agent journalctl -u pathgauge-agent --no-pager -n 30 >&2 || true; exit 1; }

build_image() { # build_image TAG BASE
    docker image inspect "$1" >/dev/null 2>&1 && return
    printf 'FROM %s\nRUN dnf install -y -q systemd iproute iproute-tc shadow-utils procps-ng >/dev/null && dnf clean all >/dev/null\nCMD ["/usr/sbin/init"]\n' "$2" | docker build -q -t "$1" - >/dev/null
}
build_image pg-sd-rocky9 rockylinux/rockylinux:9
build_image pg-sd-rocky8 rockylinux/rockylinux:8

cleanup() { docker rm -f pg-e2e-server pg-e2e-agent >/dev/null 2>&1 || true; docker network rm $NET >/dev/null 2>&1 || true; }
cleanup
trap cleanup EXIT
docker network create $NET >/dev/null
for role in server agent; do
    img=$SERVER_IMG; [ $role = agent ] && img=$AGENT_IMG
    docker run -d --name pg-e2e-$role --hostname pg-e2e-$role --network $NET --privileged --cgroupns=private \
        --tmpfs /run --tmpfs /run/lock -v "$PWD/dist:/dist:ro" "$img" >/dev/null
done
sleep 5
for role in server agent; do
    docker exec pg-e2e-$role dnf install -y -q "/dist/$(basename "$RPM")" >/dev/null || fail "install on $role"
done
echo "-- installed $(basename "$RPM") on both"

docker exec pg-e2e-server sh -c "echo '$PASS' > /root/pw && pathgauge server init --admin admin --password-file /root/pw --enable" || fail "server init"
sleep 3
docker exec pg-e2e-server systemctl is-active --quiet pathgauge-server || fail "server service not active"
TOKEN=$(docker exec pg-e2e-server pathgauge server join-token --url https://pg-e2e-server:7443 | grep -o 'pgjoin_[a-z0-9_]*' | head -1)
[ -n "$TOKEN" ] || fail "no join token"
docker exec pg-e2e-agent pathgauge agent join https://pg-e2e-server:7443 --token "$TOKEN" --name rocky8-agent --enable || fail "agent join"
sleep 4
docker exec pg-e2e-agent systemctl is-active --quiet pathgauge-agent || fail "agent service not active"
docker exec pg-e2e-agent stat -c '%U %a' /var/lib/pathgauge/agent/agent.key | grep -q "^pathgauge 600$" || fail "agent key not owned by the service user"
echo "-- services active; agent enrolled"

SERVER_IP=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" pg-e2e-server)
python3 -I - "$SERVER_IP" "$PASS" <<'PY' || fail "API checks"
import http.cookiejar, json, ssl, sys, time, urllib.request
ip, pw = sys.argv[1], sys.argv[2]
ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
jar = http.cookiejar.CookieJar()
op = urllib.request.build_opener(urllib.request.HTTPSHandler(context=ctx), urllib.request.HTTPCookieProcessor(jar))
def api(method, path, body=None):
    req = urllib.request.Request(f"https://{ip}:7443{path}", method=method, data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Content-Type": "application/json", "X-PathGauge": "1"})
    with op.open(req, timeout=30) as r:
        return json.loads(r.read() or b"null")
api("POST", "/api/auth/login", {"username": "admin", "password": pw})
for _ in range(30):
    agents = api("GET", "/api/agents")
    if agents and agents[0]["online"]:
        break
    time.sleep(1)
else:
    sys.exit("agent never came online")
a = agents[0]
print(f"   agent {a['name']} online, version {a['version']}")
tests = ["info", "latency", "trace", "tcp-down", "tcp-up", "tcp-down-1", "udp-down", "bufferbloat", "pmtu", "integrity", "sweep-down", "sqm"]
rid = api("POST", "/api/runs", {"agent_id": a["id"], "profile": "standard", "tests": tests, "scale": 0.3, "max_rate_mbps": 3000})["run_id"]
t0 = time.time()
while True:
    run = api("GET", f"/api/runs/{rid}")
    if run["status"] != "running":
        break
    if time.time() - t0 > 600:
        sys.exit("run timed out")
    time.sleep(3)
print(f"   run {rid}: {run['status']} in {time.time()-t0:.0f}s, health {run['health_score']}, down {run['down_mbps']}")
if run["status"] != "done":
    sys.exit("run failed: " + str(run.get("error")))
rep = run["report"]
bad = [f"{s['id']}: {s.get('error')}" for s in rep["steps"] if s["status"] != "ok"]
if bad:
    sys.exit("failed steps: " + "; ".join(bad))
steps = {s["id"].split("-", 1)[1]: s["result"] for s in rep["steps"]}
trace = steps.get("trace-fwd")
assert trace and trace["method"] == "udp-paris", f"service should have CAP_NET_RAW for Paris traceroute: {trace and trace['method']}"
assert steps["sqm"]["steps"], "SQM tuner did not run (shaping via the agent service)"
assert all(not s.get("error") for s in steps["sqm"]["steps"]), steps["sqm"]["steps"]
print(f"   steps ok: {len(rep['steps'])}; trace method {trace['method']}; sqm qdisc {steps['sqm']['steps'][0]['qdisc']}; findings {[f['id'] for f in rep['findings']][:6]}")
page = op.open(urllib.request.Request(f"https://{ip}:7443/agents"), timeout=30).read().decode()
assert "/assets/" in page and "dashboard not built" not in page, "the packaged server must serve the built dashboard"
print("   dashboard served (SPA shell with assets)")
ss = api("GET", f"/api/monitor/{a['id']}?hours=1")
print(f"   monitoring samples so far: {len(ss['samples'])}")
PY
# Shaping must have been removed from the agent.
docker exec pg-e2e-agent sh -c 'tc qdisc show | grep -c "5047:" || true' | grep -qx 0 || fail "shaping left on the agent"
docker exec pg-e2e-agent sh -c 'ip link show pg-ifb0 >/dev/null 2>&1' && fail "IFB device left on the agent"
echo "PASS: packaged server ($SERVER_IMG) and agent ($AGENT_IMG) work end to end under systemd"
