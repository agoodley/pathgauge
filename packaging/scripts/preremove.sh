#!/bin/sh
# deb: $1 = remove | upgrade | deconfigure | failed-upgrade
# rpm: $1 = number of package instances after this transaction (0 = erase)
set -e

case "$1" in
    remove|0)
        if [ -d /run/systemd/system ]; then
            systemctl disable --now pathgauge-server.service pathgauge-agent.service >/dev/null 2>&1 || true
        fi
        ;;
esac
