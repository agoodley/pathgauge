#!/bin/sh
# deb: $1 = configure, $2 = previously installed version (empty on first install)
# rpm: $1 = number of package instances after this transaction (1 = first install)
set -e

upgrade=false
case "$1" in
    configure) [ -n "$2" ] && upgrade=true ;;
    [2-9]) upgrade=true ;;
esac

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [ "$upgrade" = true ]; then
        # Restart only the units that were already running.
        systemctl try-restart pathgauge-server.service pathgauge-agent.service >/dev/null 2>&1 || true
    fi
fi

if [ "$upgrade" = false ]; then
    cat <<'MSG'

PathGauge installed. Nothing is enabled until you choose a role:
  Check this host:  sudo pathgauge doctor
  Server box:       sudo pathgauge server init --enable
                    then open https://<this-host>:7443 and create a join token
  Agent box:        sudo pathgauge agent join https://<server>:7443 --token <token> --enable
  Quick peer test:  pathgauge endpoint  (on one box)  +  pathgauge test <host> --key <key>
Configuration lives in /etc/pathgauge/. Only test networks you are authorised to test.

MSG
fi
