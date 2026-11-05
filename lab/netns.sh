#!/usr/bin/env bash
# PathGauge lab: a reproducible test path built from network namespaces.
#
#   pg-a ──────── pg-ra ═══════════ pg-rb ──────── pg-b
#   10.77.1.2     10.77.1.1         10.77.2.1      10.77.2.2
#   fd77:1::2     10.77.0.1 ─ 10.77.0.2            fd77:2::2
#
# Impairments never touch the endpoints (qdiscs on a sending host distort
# TCP through TCP small queues). For traffic from A to B:
#   - delay, jitter and loss: netem on pg-ra, egress towards pg-rb (the "WAN")
#   - bottleneck rate and buffer: on pg-rb, egress towards pg-b (the "access link")
# B to A mirrors this (netem on pg-rb, bottleneck on pg-ra).
#
# Usage (needs root):
#   lab/netns.sh up                      create the topology
#   lab/netns.sh down                    remove it
#   lab/netns.sh status                  show addresses and qdiscs
#   lab/netns.sh impair a2b|b2a [options]
#       --delay 20ms     one-way delay (so RTT gains 2x if set both ways)
#       --jitter 2ms     delay variation (needs --delay)
#       --loss 0.5%      random loss
#       --rate 100mbit   bottleneck rate (kbit, mbit or gbit)
#       --buffer 50ms    bottleneck queue depth in time (tbf/htb only; default 50ms)
#       --qdisc tbf      bottleneck: tbf (FIFO), cake, fq_codel, police (drop
#                        above the rate with no queue; burst = --buffer at rate),
#                        or prio (tbf with strict priority for DSCP EF)
#       --reorder 25%    reorder this share of packets (needs --delay)
#   lab/netns.sh clear [a2b|b2a]         remove impairments
#   lab/netns.sh nat on|off              put B (the client) behind NAT on pg-rb:
#       IPv4 is masqueraded to 10.77.0.2, and unsolicited inbound traffic to
#       B is dropped for IPv4 and IPv6, like a typical home or office router.
#       Needs nftables.
#   lab/netns.sh mtu N                   set the MTU of the link between the routers
#   lab/netns.sh icmp-block on|off       routers drop ICMP "fragmentation needed" /
#                                        "packet too big" (makes a PMTU black hole)
#   lab/netns.sh mss-clamp N|off         routers rewrite the TCP MSS of SYNs to N
#   lab/netns.sh dscp-bleach on|off      routers reset DSCP to best effort
#   lab/netns.sh reroute                 re-add routes everywhere (drops cached path MTUs)
#   lab/netns.sh exec a|b|ra|rb CMD...   run a command inside a namespace
set -euo pipefail

NS_A=pg-a NS_RA=pg-ra NS_RB=pg-rb NS_B=pg-b

die() { echo "netns.sh: $*" >&2; exit 1; }
ns_exists() { ip netns list | awk '{print $1}' | grep -qx "$1"; }
in_ns() { local ns=$1; shift; ip netns exec "$ns" "$@"; }

ns_name() {
    case "$1" in
        a) echo $NS_A ;; b) echo $NS_B ;; ra) echo $NS_RA ;; rb) echo $NS_RB ;;
        *) die "unknown namespace '$1' (want a, b, ra or rb)" ;;
    esac
}

# Disable segmentation offloads on an interface so router qdiscs see
# wire-sized packets, as a real router would. Skipped if ethtool is absent.
no_offload() {
    command -v ethtool >/dev/null 2>&1 || return 0
    in_ns "$1" ethtool -K "$2" tso off gso off gro off >/dev/null 2>&1 || true
}

link() { # link NS1 IF1 NS2 IF2
    ip link add "$2" netns "$1" type veth peer name "$4" netns "$3"
    in_ns "$1" ip link set "$2" up
    in_ns "$3" ip link set "$4" up
}

up() {
    ns_exists $NS_A && die "lab already up (run: $0 down)"
    for ns in $NS_A $NS_RA $NS_RB $NS_B; do
        ip netns add "$ns"
        in_ns "$ns" ip link set lo up
    done

    link $NS_A a0 $NS_RA ra-a
    link $NS_RA ra-rb $NS_RB rb-ra
    link $NS_RB rb-b $NS_B b0

    in_ns $NS_A  ip addr add 10.77.1.2/24 dev a0
    in_ns $NS_RA ip addr add 10.77.1.1/24 dev ra-a
    in_ns $NS_RA ip addr add 10.77.0.1/30 dev ra-rb
    in_ns $NS_RB ip addr add 10.77.0.2/30 dev rb-ra
    in_ns $NS_RB ip addr add 10.77.2.1/24 dev rb-b
    in_ns $NS_B  ip addr add 10.77.2.2/24 dev b0

    in_ns $NS_A  ip -6 addr add fd77:1::2/64 dev a0 nodad
    in_ns $NS_RA ip -6 addr add fd77:1::1/64 dev ra-a nodad
    in_ns $NS_RA ip -6 addr add fd77::1/64 dev ra-rb nodad
    in_ns $NS_RB ip -6 addr add fd77::2/64 dev rb-ra nodad
    in_ns $NS_RB ip -6 addr add fd77:2::1/64 dev rb-b nodad
    in_ns $NS_B  ip -6 addr add fd77:2::2/64 dev b0 nodad

    in_ns $NS_A  ip route add default via 10.77.1.1
    in_ns $NS_B  ip route add default via 10.77.2.1
    in_ns $NS_RA ip route add 10.77.2.0/24 via 10.77.0.2
    in_ns $NS_RB ip route add 10.77.1.0/24 via 10.77.0.1
    in_ns $NS_A  ip -6 route add default via fd77:1::1
    in_ns $NS_B  ip -6 route add default via fd77:2::1
    in_ns $NS_RA ip -6 route add fd77:2::/64 via fd77::2
    in_ns $NS_RB ip -6 route add fd77:1::/64 via fd77::1

    for ns in $NS_RA $NS_RB; do
        in_ns "$ns" sysctl -qw net.ipv4.ip_forward=1
        in_ns "$ns" sysctl -qw net.ipv6.conf.all.forwarding=1
    done
    for pair in "$NS_RA ra-a" "$NS_RA ra-rb" "$NS_RB rb-ra" "$NS_RB rb-b"; do
        # shellcheck disable=SC2086
        no_offload $pair
    done

    echo "lab up: A=10.77.1.2 / fd77:1::2 (ns $NS_A)  B=10.77.2.2 / fd77:2::2 (ns $NS_B)"
}

down() {
    rm -rf "$STATE_DIR"
    for ns in $NS_A $NS_RA $NS_RB $NS_B; do
        ns_exists "$ns" && ip netns del "$ns"
    done
    echo "lab down"
}

nat() {
    ns_exists $NS_A || die "lab is not up"
    command -v nft >/dev/null || die "nftables (nft) is required for NAT mode"
    in_ns $NS_RB nft delete table ip pglab_nat 2>/dev/null || true
    in_ns $NS_RB nft delete table inet pglab_fw 2>/dev/null || true
    case "${1:-}" in
        on)
            in_ns $NS_RB nft -f - <<'NFT'
table ip pglab_nat {
    chain postrouting {
        type nat hook postrouting priority 100; policy accept;
        oifname "rb-ra" ip saddr 10.77.2.0/24 masquerade
    }
}
table inet pglab_fw {
    chain forward {
        type filter hook forward priority 0; policy accept;
        iifname "rb-ra" oifname "rb-b" ct state established,related accept
        iifname "rb-ra" oifname "rb-b" drop
    }
}
NFT
            echo "nat on: B is reached from A as 10.77.0.2; inbound connections to B are dropped"
            ;;
        off) echo "nat off" ;;
        *) die "usage: $0 nat on|off" ;;
    esac
}

status() {
    ns_exists $NS_A || die "lab is not up"
    for ns in $NS_A $NS_RA $NS_RB $NS_B; do
        echo "== $ns"
        in_ns "$ns" ip -brief addr show | grep -v '^lo'
        in_ns "$ns" tc qdisc show | grep -v 'noqueue' | sed 's/^/   qdisc: /' || true
    done
    if in_ns $NS_RB nft list table ip pglab_nat >/dev/null 2>&1; then echo "nat: on (B behind pg-rb)"; else echo "nat: off"; fi
}

# direction_ifaces DIR -> "<wan ns> <wan if> <access ns> <access if>"
direction_ifaces() {
    case "$1" in
        a2b) echo "$NS_RA ra-rb $NS_RB rb-b" ;;
        b2a) echo "$NS_RB rb-ra $NS_RA ra-a" ;;
        *) die "direction must be a2b or b2a" ;;
    esac
}

# rate_bps "100mbit" -> 100000000
rate_bps() {
    local v=$1 n unit
    n=${v%%[a-zA-Z]*}
    unit=${v#"$n"}
    [[ $n =~ ^[0-9]+$ ]] || die "invalid rate '$v' (examples: 500kbit, 100mbit, 1gbit)"
    case "$unit" in
        kbit) echo $((n * 1000)) ;;
        mbit) echo $((n * 1000000)) ;;
        gbit) echo $((n * 1000000000)) ;;
        *) die "invalid rate unit in '$v' (use kbit, mbit or gbit)" ;;
    esac
}

# ms_of "50ms" -> 50
ms_of() {
    [[ $1 =~ ^([0-9]+)ms$ ]] || die "invalid duration '$1' (example: 50ms)"
    echo "${BASH_REMATCH[1]}"
}

clear_dir() {
    local wan_ns wan_if acc_ns acc_if
    read -r wan_ns wan_if acc_ns acc_if <<<"$(direction_ifaces "$1")"
    in_ns "$wan_ns" tc qdisc del dev "$wan_if" root 2>/dev/null || true
    in_ns "$acc_ns" tc qdisc del dev "$acc_if" root 2>/dev/null || true
    in_ns "$acc_ns" tc qdisc del dev "$acc_if" clsact 2>/dev/null || true
}

# router_rules: per-router nftables table holding the optional middlebox
# behaviours (icmp-block, mss-clamp, dscp-bleach). State lives in files so
# each toggle rebuilds the table from scratch.
STATE_DIR=/run/pathgauge-lab
router_rules() {
    command -v nft >/dev/null || die "nftables (nft) is required"
    mkdir -p "$STATE_DIR"
    local rules=""
    [ -f "$STATE_DIR/icmp-block" ] && rules+="        icmp type destination-unreachable icmp code frag-needed drop
        icmpv6 type packet-too-big drop
"
    [ -f "$STATE_DIR/mss" ] && rules+="        tcp flags syn tcp option maxseg size set $(cat "$STATE_DIR/mss")
"
    [ -f "$STATE_DIR/dscp-bleach" ] && rules+="        ip dscp != cs0 ip dscp set cs0
        ip6 dscp != cs0 ip6 dscp set cs0
"
    # Routers originate "fragmentation needed" themselves (OUTPUT hook);
    # forwarded ICMP from further away passes FORWARD. Block both.
    local out_rules=""
    [ -f "$STATE_DIR/icmp-block" ] && out_rules="        icmp type destination-unreachable icmp code frag-needed drop
        icmpv6 type packet-too-big drop
"
    for ns in $NS_RA $NS_RB; do
        in_ns "$ns" nft delete table inet pglab_mbox 2>/dev/null || true
        [ -z "$rules$out_rules" ] && continue
        in_ns "$ns" nft -f - <<NFT
table inet pglab_mbox {
    chain forward {
        type filter hook forward priority -150; policy accept;
$rules    }
    chain output {
        type filter hook output priority -150; policy accept;
$out_rules    }
}
NFT
    done
}

toggle() { # toggle NAME on|off [value]
    mkdir -p "$STATE_DIR"
    case "$2" in
        on) echo "${3:-1}" > "$STATE_DIR/$1" ;;
        off) rm -f "$STATE_DIR/$1" ;;
        *) die "usage: $0 $1 on|off" ;;
    esac
    router_rules
}

impair() {
    ns_exists $NS_A || die "lab is not up"
    local dir=${1:-}; shift || true
    local delay="" jitter="" loss="" rate="" buffer="50ms" qdisc="tbf" reorder=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --delay) delay=$2; shift 2 ;;
            --jitter) jitter=$2; shift 2 ;;
            --loss) loss=$2; shift 2 ;;
            --rate) rate=$2; shift 2 ;;
            --buffer) buffer=$2; shift 2 ;;
            --qdisc) qdisc=$2; shift 2 ;;
            --reorder) reorder=$2; shift 2 ;;
            *) die "unknown option '$1'" ;;
        esac
    done
    [ -n "$jitter" ] && [ -z "$delay" ] && die "--jitter needs --delay"

    local wan_ns wan_if acc_ns acc_if
    read -r wan_ns wan_if acc_ns acc_if <<<"$(direction_ifaces "$dir")"
    clear_dir "$dir"

    if [ -n "$delay$loss" ]; then
        local args=(netem limit 1000000)
        [ -n "$delay" ] && args+=(delay "$delay")
        [ -n "$jitter" ] && args+=("$jitter" distribution normal)
        [ -n "$loss" ] && args+=(loss "$loss")
        [ -n "$reorder" ] && args+=(reorder "$reorder" 50%)
        in_ns "$wan_ns" tc qdisc add dev "$wan_if" root "${args[@]}"
    fi

    if [ -n "$rate" ]; then
        local bps buf_ms burst limit
        bps=$(rate_bps "$rate")
        buf_ms=$(ms_of "$buffer")
        # Burst of ~1 ms at line rate, never below 4 full-size frames.
        burst=$((bps / 8 / 1000)); [ $burst -lt 6056 ] && burst=6056
        # Queue limit in bytes equivalent to the buffer depth.
        limit=$((bps * buf_ms / 8000)); [ $limit -lt 3028 ] && limit=3028
        case "$qdisc" in
            tbf)
                in_ns "$acc_ns" tc qdisc add dev "$acc_if" root tbf rate "$rate" burst "$burst" limit "$limit"
                ;;
            cake)
                in_ns "$acc_ns" tc qdisc add dev "$acc_if" root cake bandwidth "$rate"
                ;;
            prio)
                # Rate-limited FIFO for everything except DSCP EF, which
                # gets a strict-priority band (an ideal QoS policy).
                in_ns "$acc_ns" tc qdisc add dev "$acc_if" root handle 1: tbf rate "$rate" burst "$burst" limit "$limit"
                in_ns "$acc_ns" tc qdisc add dev "$acc_if" parent 1:1 handle 10: prio bands 3 priomap 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1
                in_ns "$acc_ns" tc filter add dev "$acc_if" parent 10: protocol ip u32 match ip dsfield 0xb8 0xfc flowid 10:1
                ;;
            police)
                in_ns "$acc_ns" tc qdisc add dev "$acc_if" clsact
                in_ns "$acc_ns" tc filter add dev "$acc_if" egress matchall action police rate "$rate" burst "$limit" conform-exceed drop
                ;;
            fq_codel)
                in_ns "$acc_ns" tc qdisc add dev "$acc_if" root handle 1: htb default 1
                in_ns "$acc_ns" tc class add dev "$acc_if" parent 1: classid 1:1 htb rate "$rate" ceil "$rate" burst "$burst" quantum 1514
                in_ns "$acc_ns" tc qdisc add dev "$acc_if" parent 1:1 fq_codel
                ;;
            *) die "unknown --qdisc '$qdisc' (want tbf, cake, fq_codel, police or prio)" ;;
        esac
    fi
    local bottleneck="-"
    if [ -n "$rate" ]; then
        bottleneck="$rate $qdisc"
        [ "$qdisc" = tbf ] && bottleneck+=" buffer $buffer"
    fi
    echo "impaired $dir: delay=${delay:--} jitter=${jitter:--} loss=${loss:--} bottleneck=$bottleneck"
}

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; }

cmd=${1:-}; shift || true
case "$cmd" in
    up|down|status|impair|clear|nat|exec|mtu|icmp-block|mss-clamp|dscp-bleach|reroute) ;;
    -h|--help|help) usage; exit 0 ;;
    *) usage; exit 2 ;;
esac
[ "$(id -u)" -eq 0 ] || die "needs root (try: sudo $0 $cmd $*)"
command -v ip >/dev/null || die "iproute2 is required"

case "$cmd" in
    up) up ;;
    down) down ;;
    status) status ;;
    impair) impair "$@" ;;
    nat) nat "$@" ;;
    mtu)
        [[ ${1:-} =~ ^[0-9]+$ ]] || die "usage: $0 mtu N"
        in_ns $NS_RA ip link set ra-rb mtu "$1"
        in_ns $NS_RB ip link set rb-ra mtu "$1"
        echo "router link MTU $1"
        ;;
    icmp-block) toggle icmp-block "${1:-}"; echo "icmp-block ${1:-}" ;;
    reroute)
        # Deleting and re-adding a route discards its cached PMTU exceptions.
        in_ns $NS_A ip route replace default via 10.77.1.1 && in_ns $NS_A ip route del default && in_ns $NS_A ip route add default via 10.77.1.1
        in_ns $NS_B ip route del default && in_ns $NS_B ip route add default via 10.77.2.1
        in_ns $NS_RA ip route del 10.77.2.0/24 && in_ns $NS_RA ip route add 10.77.2.0/24 via 10.77.0.2
        in_ns $NS_RB ip route del 10.77.1.0/24 && in_ns $NS_RB ip route add 10.77.1.0/24 via 10.77.0.1
        echo "routes re-added"
        ;;
    dscp-bleach) toggle dscp-bleach "${1:-}"; echo "dscp-bleach ${1:-}" ;;
    mss-clamp)
        if [ "${1:-}" = off ]; then toggle mss off; else
            [[ ${1:-} =~ ^[0-9]+$ ]] || die "usage: $0 mss-clamp N|off"
            toggle mss on "$1"
        fi
        echo "mss-clamp ${1:-}"
        ;;
    clear)
        if [ $# -gt 0 ]; then clear_dir "$1"; else clear_dir a2b; clear_dir b2a; fi
        echo "cleared ${1:-a2b b2a}"
        ;;
    exec)
        [ $# -ge 2 ] || die "usage: $0 exec a|b|ra|rb CMD..."
        ns=$(ns_name "$1"); shift
        exec ip netns exec "$ns" "$@"
        ;;
esac
