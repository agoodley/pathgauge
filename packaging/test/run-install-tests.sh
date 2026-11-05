#!/bin/sh
# Run install-test.sh against every supported distro in Docker, using the
# amd64 packages in dist/ (build them first with `make snapshot`).
set -eu
cd "$(dirname "$0")/../.."

deb=$(find dist -name "*_amd64.deb" 2>/dev/null | head -1)
rpm=$(find dist -name "*.x86_64.rpm" 2>/dev/null | head -1)
[ -n "$deb" ] && [ -n "$rpm" ] || { echo "no packages in dist/; run: make snapshot" >&2; exit 1; }

failed=0
for spec in "ubuntu:22.04 $deb" "ubuntu:24.04 $deb" "ubuntu:26.04 $deb" \
            "rockylinux/rockylinux:8 $rpm" "rockylinux/rockylinux:9 $rpm" "rockylinux/rockylinux:10 $rpm"; do
    # shellcheck disable=SC2086 # split "image package" on purpose
    set -- $spec
    if docker run --rm -v "$PWD/dist:/dist:ro" -v "$PWD/packaging/test:/t:ro" "$1" \
        /t/install-test.sh "/dist/$(basename "$2")" >/tmp/pathgauge-install-$$.log 2>&1; then
        echo "PASS  $1"
    else
        echo "FAIL  $1"; sed 's/^/      /' /tmp/pathgauge-install-$$.log | tail -15; failed=1
    fi
done
rm -f /tmp/pathgauge-install-$$.log
exit $failed
