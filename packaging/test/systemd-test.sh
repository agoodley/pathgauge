#!/bin/sh
# Start both services under a real systemd and check they run unprivileged
# with exactly the intended capabilities. Run as root inside a container
# booted with systemd as PID 1. Usage: systemd-test.sh /path/to/package
set -eu
pkg=$1
fail() { echo "FAIL: $*" >&2; journalctl --no-pager -n 30 -u "pathgauge-*" >&2 || true; exit 1; }

# Wait for systemd to finish booting.
for _ in $(seq 1 30); do
    state=$(systemctl is-system-running 2>/dev/null || true)
    case "$state" in running|degraded) break ;; esac
    sleep 1
done

if command -v dnf >/dev/null 2>&1; then
    dnf install -y -q "$pkg" >/dev/null
else
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg" >/dev/null 2>&1
fi
# shellcheck source=/dev/null
. /etc/os-release
echo "== $PRETTY_NAME"

# The agent only runs once enrolled, so set up the server and enrol the
# agent with it through the CLI first (as an administrator would).
echo 'systemd-test-password' > /root/pw
pathgauge server init --admin admin --password-file /root/pw --enable >/dev/null || fail "server init"
for _ in $(seq 1 20); do
    systemctl is-active --quiet pathgauge-server.service && break
    sleep 1
done
token=$(pathgauge server join-token --url "https://$(hostname):7443" | grep -o 'pgjoin_[a-z0-9_]*' | head -1)
[ -n "$token" ] || fail "no join token"
pathgauge agent join "https://$(hostname):7443" --token "$token" --name systemd-test >/dev/null || fail "agent join"

for role in server agent; do
    unit=pathgauge-$role.service
    systemctl enable --now "$unit" >/dev/null 2>&1
    sleep 2
    systemctl is-active --quiet "$unit" || fail "$unit is not active"
    pid=$(systemctl show -p MainPID --value "$unit")
    user=$(ps -o user= -p "$pid" | tr -d ' ')
    [ "$user" = pathgauge ] || fail "$unit runs as '$user', want pathgauge"
    capeff=$(awk '/^CapEff/ {print $2}' "/proc/$pid/status")
    # NET_ADMIN+NET_RAW; the server may also bind ports below 1024 (443).
    want=0000000000003000
    [ "$role" = server ] && want=0000000000003400
    [ "$capeff" = "$want" ] || fail "$unit CapEff=$capeff, want $want"
    journalctl --no-pager -u "$unit" | grep -q "$role starting" || fail "$unit did not log its start-up"
    echo "$unit: active, user=$user, CapEff=$capeff"
done

[ "$(stat -c '%U:%G %a' /var/lib/pathgauge)" = "pathgauge:pathgauge 750" ] || fail "state directory ownership wrong"

systemctl stop pathgauge-agent.service pathgauge-server.service
for role in agent server; do
    result=$(systemctl show -p Result --value "pathgauge-$role.service")
    [ "$result" = success ] || fail "pathgauge-$role did not stop cleanly (result: $result)"
done
echo "PASS: services start, run unprivileged with only the intended capabilities, and stop cleanly"
