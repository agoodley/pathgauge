#!/usr/bin/env bash
# Smoke test for the lab: builds the topology, applies known impairments and
# checks that the measured path matches them. Needs root, iproute2, ping and
# (for the rate check) iperf3. Used by CI and `make lab-smoke`.
set -euo pipefail
cd "$(dirname "$0")"

fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() { ./netns.sh down >/dev/null 2>&1 || true; }
trap cleanup EXIT

./netns.sh down >/dev/null 2>&1 || true
./netns.sh up

echo "-- reachability (IPv4 and IPv6)"
./netns.sh exec a ping -c 2 -W 2 10.77.2.2 >/dev/null || fail "A cannot reach B over IPv4"
./netns.sh exec b ping -c 2 -W 2 10.77.1.2 >/dev/null || fail "B cannot reach A over IPv4"
./netns.sh exec a ping -6 -c 2 -W 2 fd77:2::2 >/dev/null || fail "A cannot reach B over IPv6"

echo "-- delay: 15 ms each way should give an RTT of ~30 ms"
./netns.sh impair a2b --delay 15ms
./netns.sh impair b2a --delay 15ms
avg=$(./netns.sh exec a ping -c 10 -i 0.2 -q 10.77.2.2 | awk -F/ '/^rtt|^round-trip/ {print $5}')
echo "   average RTT ${avg} ms"
awk -v r="$avg" 'BEGIN { exit !(r >= 29 && r <= 34) }' || fail "RTT ${avg} ms outside 29-34 ms"

if command -v iperf3 >/dev/null 2>&1; then
    echo "-- bottleneck: 50 Mbit/s A to B should measure 45-50 Mbit/s"
    ./netns.sh impair a2b --delay 15ms --rate 50mbit --buffer 50ms
    ./netns.sh exec b iperf3 -s -1 -D >/dev/null
    sleep 0.5
    mbps=$(./netns.sh exec a iperf3 -c 10.77.2.2 -t 5 -O 1 -J |
        python3 -c 'import json,sys; print(round(json.load(sys.stdin)["end"]["sum_received"]["bits_per_second"]/1e6, 1))')
    echo "   measured ${mbps} Mbit/s"
    awk -v r="$mbps" 'BEGIN { exit !(r >= 45 && r <= 50.5) }' || fail "throughput ${mbps} Mbit/s outside 45-50.5"
else
    echo "-- iperf3 not installed: skipping the bottleneck check"
fi

if command -v nft >/dev/null 2>&1; then
    echo "-- NAT: B behind a NAT router, as a client often is"
    ./netns.sh clear >/dev/null
    ./netns.sh nat on
    ./netns.sh exec a ping -c 1 -W 1 10.77.2.2 >/dev/null 2>&1 && fail "A reached B through NAT"
    ./netns.sh exec a ping -6 -c 1 -W 1 fd77:2::2 >/dev/null 2>&1 && fail "A reached B over IPv6 through the firewall"
    ./netns.sh exec b ping -c 1 -W 2 10.77.1.2 >/dev/null || fail "B cannot reach A through NAT"
    # The data plane relies on this: the server replies to the address and
    # port it observes, and the reply must get back through the NAT.
    ./netns.sh exec a python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("10.77.1.2", 9999)); s.settimeout(5)
data, peer = s.recvfrom(64)
open("/tmp/pg-nat-peer", "w").write(peer[0])
s.sendto(b"reply", peer)' &
    sleep 0.5
    reply=$(./netns.sh exec b python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
s.sendto(b"hello", ("10.77.1.2", 9999))
print(s.recvfrom(64)[0].decode())') || fail "UDP reply did not get back through the NAT"
    wait
    peer=$(cat /tmp/pg-nat-peer)
    echo "   A saw B as ${peer}; B received '${reply}'"
    [ "$peer" = 10.77.0.2 ] || fail "A saw B as $peer, want the NAT address 10.77.0.2"
    [ "$reply" = reply ] || fail "unexpected reply '$reply'"
    ./netns.sh nat off
    ./netns.sh exec a ping -c 1 -W 2 10.77.2.2 >/dev/null || fail "A cannot reach B after NAT off"
else
    echo "-- nftables not installed: skipping the NAT check"
fi

echo "-- clear"
./netns.sh clear
./netns.sh status >/dev/null
echo "lab smoke test passed"
