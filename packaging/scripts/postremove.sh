#!/bin/sh
# deb: $1 = remove | purge | upgrade | ...
# rpm: $1 = number of package instances after this transaction (0 = erase)
# Data in /var/lib/pathgauge is kept unless the deb package is purged.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [ "$1" = purge ]; then
    rm -rf /var/lib/pathgauge /etc/pathgauge
fi
