#!/bin/sh
# Install, inspect and remove a PathGauge package inside a disposable
# Rocky or Ubuntu container. Usage: install-test.sh /path/to/package.{rpm,deb}
# Run as root. Used by CI and `make install-test`.
set -eu

pkg=$1
fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- $*"; }

if command -v dnf >/dev/null 2>&1; then
    family=rpm
    install() { dnf install -y -q "$@" >/dev/null; }
    reinstall() { dnf reinstall -y -q "$@" >/dev/null; }
    remove() { dnf remove -y -q pathgauge >/dev/null; }
    install systemd >/dev/null 2>&1 || true   # for systemd-analyze
else
    family=deb
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq >/dev/null
    install() { apt-get install -y -qq "$@" >/dev/null; }
    reinstall() { apt-get install --reinstall -y -qq "$@" >/dev/null; }
    remove() { apt-get remove -y -qq pathgauge >/dev/null; }
    install systemd >/dev/null 2>&1 || true
fi
# shellcheck source=/dev/null
. /etc/os-release
echo "== $PRETTY_NAME ($family): $(basename "$pkg")"

step "install"
install "$pkg"
command -v pathgauge >/dev/null || fail "binary not on PATH"
pathgauge version

step "service account"
getent passwd pathgauge >/dev/null || fail "user pathgauge missing"
getent group pathgauge >/dev/null || fail "group pathgauge missing"

step "file ownership and modes"
check_mode() { # check_mode PATH "owner:group mode"
    got=$(stat -c '%U:%G %a' "$1")
    [ "$got" = "$2" ] || fail "$1 is '$got', want '$2'"
}
check_mode /etc/pathgauge "root:pathgauge 750"
for f in server agent policy; do
    check_mode /etc/pathgauge/$f.yaml "root:pathgauge 640"
done
for f in /usr/lib/systemd/system/pathgauge-server.service \
         /usr/lib/systemd/system/pathgauge-agent.service \
         /usr/share/bash-completion/completions/pathgauge \
         /usr/share/licenses/pathgauge/LICENSE; do
    [ -f "$f" ] || fail "$f missing"
done

step "configuration readable by the service user"
runuser -u pathgauge -- pathgauge server run --check-config
runuser -u pathgauge -- pathgauge agent run --check-config

step "systemd units"
if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze verify /usr/lib/systemd/system/pathgauge-server.service \
        /usr/lib/systemd/system/pathgauge-agent.service
    echo "units verified"
else
    echo "systemd-analyze unavailable; skipped"
fi

step "doctor"
pathgauge doctor --json >/dev/null

step "config files survive edits on reinstall (noreplace)"
echo "# local edit" >> /etc/pathgauge/server.yaml
reinstall "$pkg"
grep -q "# local edit" /etc/pathgauge/server.yaml || fail "local config edit lost on reinstall"

step "remove"
remove
[ -e /usr/bin/pathgauge ] && fail "binary still installed after removal"
getent passwd pathgauge >/dev/null || fail "service user should be kept after removal"
echo "PASS: $PRETTY_NAME"
