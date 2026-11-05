#!/bin/sh
# Create the unprivileged service account before files are unpacked, so
# /etc/pathgauge can be group-owned by it.
set -e

if ! getent group pathgauge >/dev/null 2>&1; then
    groupadd --system pathgauge
fi
if ! getent passwd pathgauge >/dev/null 2>&1; then
    useradd --system --gid pathgauge --no-create-home \
        --home-dir /var/lib/pathgauge --shell /usr/sbin/nologin \
        --comment "PathGauge service" pathgauge
fi
