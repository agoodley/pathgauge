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

for role in server agent; do
    unit=pathgauge-$role.service
    systemctl enable --now "$unit" >/dev/null 2>&1
    sleep 2
    systemctl is-active --quiet "$unit" || fail "$unit is not active"
    pid=$(systemctl show -p MainPID --value "$unit")
    user=$(ps -o user= -p "$pid" | tr -d ' ')
    [ "$user" = pathgauge ] || fail "$unit runs as '$user', want pathgauge"
    capeff=$(awk '/^CapEff/ {print $2}' "/proc/$pid/status")
    [ "$capeff" = 0000000000003000 ] || fail "$unit CapEff=$capeff, want 0000000000003000 (NET_ADMIN+NET_RAW)"
    journalctl --no-pager -u "$unit" | grep -q "$role starting" || fail "$unit did not log its start-up"
    echo "$unit: active, user=$user, CapEff=$capeff"
done

[ "$(stat -c '%U:%G %a' /var/lib/pathgauge)" = "pathgauge:pathgauge 750" ] || fail "state directory ownership wrong"

systemctl stop pathgauge-server.service pathgauge-agent.service
journalctl --no-pager -u pathgauge-agent.service | grep -q "agent stopping" || fail "agent did not shut down cleanly"
echo "PASS: services start, run unprivileged with CAP_NET_ADMIN+CAP_NET_RAW, and stop cleanly"
