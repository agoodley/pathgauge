#!/usr/bin/env bash
# PathGauge lab scenarios: each one builds a known network fault in the
# netns lab, runs PathGauge across it, and checks that the measurements and
# the diagnosis match the fault. This is the main accuracy test.
#
# Usage (as root, normally inside the lab container via `make lab-scenarios`):
#   lab/scenarios.sh /path/to/pathgauge [SCENARIO ...]
# Results JSON files are kept in $OUT (default /tmp/pathgauge-lab).
set -uo pipefail
cd "$(dirname "$0")" || exit 1

PG=${1:?usage: $0 /path/to/pathgauge [scenario ...]}
shift
OUT=${OUT:-/tmp/pathgauge-lab}
KEY=labkey
A=10.77.1.2
A6=fd77:1::2
mkdir -p "$OUT"

lab() { ./netns.sh "$@" >/dev/null; }
reset_lab() {
    lab clear
    lab nat off || true
    lab mtu 1500
    lab icmp-block off
    lab mss-clamp off
    lab dscp-bleach off
    lab reroute
}
client() { # client NAME ARGS... : run pathgauge test from B
    local name=$1; shift
    ./netns.sh exec b "$PG" test "$A" --key "$KEY" --no-resolve -q --json "$OUT/$name.json" "$@" > "$OUT/$name.log" 2>&1
    local rc=$?
    [ $rc -ne 0 ] && { echo "    pathgauge exited $rc:"; tail -5 "$OUT/$name.log" | sed 's/^/      /'; }
    return $rc
}
check() { python3 -I ./check.py "$OUT/$1.json" "${@:2}"; }

PASSED=() FAILED=()
run() { # run NAME: call scenario_NAME and record the outcome
    local name=$1
    echo "== $name"
    reset_lab
    local start=$SECONDS
    if "scenario_$name"; then PASSED+=("$name"); echo "   ok ($((SECONDS - start))s)"
    else FAILED+=("$name"); echo "   FAILED ($((SECONDS - start))s)"; fi
}

# --- scenarios -------------------------------------------------------------

scenario_clean() {
    # A healthy 500 Mbit/s path with a well-managed queue. Without a rate
    # limit the lab is CPU-bound and one stream gets an erratic share of
    # the forwarding CPU, which reads as a per-flow cap.
    lab impair a2b --delay 5ms --rate 500mbit --qdisc fq_codel
    lab impair b2a --delay 5ms --rate 500mbit --qdisc fq_codel
    client clean --tests info,latency,trace,tcp-down,tcp-up,tcp-down-1,tcp-up-1,pmtu,integrity --scale 0.5 || return 1
    check clean 'steps_ok()' \
        'approx(res("latency")["udp_rtt_ms"]["median"], 10, 15)' \
        'res("latency")["loss"]["loss_pct"] == 0' \
        'len(res("trace", "up")["hops"]) == 3 and res("trace", "up")["reached"]' \
        'len(res("trace", "down")["hops"]) == 3' \
        'res("tcp", "down", 8)["mbps"] > 300 and res("tcp", "up", 8)["mbps"] > 300' \
        'res("pmtu")["up"]["mtu"] == 1500 and res("pmtu")["down"]["mtu"] == 1500' \
        'not res("pmtu")["up"]["blackhole"] and not res("pmtu")["down"]["blackhole"]' \
        'all(d["status"] == "preserved" for d in res("integrity")["dscp_up"] + res("integrity")["dscp_down"])' \
        'res("integrity")["ecn_up"] == "preserved" and res("integrity")["ecn_down"] == "preserved"' \
        'not res("integrity")["nat"] and res("integrity")["hops_up"] == 2 and res("integrity")["hops_down"] == 2' \
        'not res("integrity")["mss_clamped_up"] and not res("integrity").get("stripped_tcp_options")' \
        'not any(f["severity"] in ("warning", "critical") for f in R["findings"])' \
        '(not res("info")["owd_trustworthy"]) or approx(res("latency")["owd_down_ms"]["median"], 5, 30)'
}

scenario_bufferbloat() {
    lab impair a2b --delay 10ms --rate 50mbit --buffer 300ms
    lab impair b2a --delay 10ms --rate 10mbit --buffer 500ms
    client bloat --tests latency,tcp-down,tcp-up,bufferbloat,sweep-down --scale 0.6 || return 1
    # Reference measurement with iperf3 over the same path.
    ./netns.sh exec a iperf3 -s -1 -D -B "$A" >/dev/null 2>&1; sleep 0.5
    local ref
    ref=$(./netns.sh exec b iperf3 -c "$A" -R -P 8 -t 6 -O 2 -J | python3 -c 'import json,sys; print(json.load(sys.stdin)["end"]["sum_received"]["bits_per_second"]/1e6)')
    echo "    iperf3 reference (download, 8 streams): ${ref} Mbit/s"
    check bloat 'steps_ok()' \
        'approx(res("tcp", "down")["mbps"], 47.8, 6)' \
        "approx(res('tcp', 'down')['mbps'], $ref, 5)" \
        'approx(res("tcp", "up")["mbps"], 9.5, 8)' \
        'grade_rank(res("bufferbloat")["grades"]["down"]) >= 3' \
        'grade_rank(res("bufferbloat")["grades"]["up"]) >= 4' \
        'res("bufferbloat")["increase_ms"]["up"] > 250' \
        'res("bufferbloat")["queueing"] == "fifo"' \
        'finding("bufferbloat.down") and finding("bufferbloat.up")' \
        'res("sweep", "down")["signature"] == "bufferbloat" and res("sweep", "down")["max_queue_ms"] > 150' \
        'approx(res("sweep", "down")["capacity_mbps"], 50, 10)'
}

scenario_cake() {
    lab impair a2b --delay 10ms --rate 50mbit --qdisc cake
    lab impair b2a --delay 10ms --rate 10mbit --qdisc cake
    client cake --tests latency,bufferbloat --scale 0.6 || return 1
    check cake 'steps_ok()' \
        'grade_rank(res("bufferbloat")["grade"]) <= 1' \
        'res("bufferbloat")["queueing"] in ("flow-queued", "low-latency")' \
        'finding("bufferbloat.good")' \
        'not finding("bufferbloat.down") and not finding("bufferbloat.up")'
}

scenario_policer() {
    lab impair a2b --delay 10ms --rate 30mbit --qdisc police --buffer 20ms
    lab impair b2a --delay 10ms
    client policer --tests sweep-down --max-rate 60 --scale 0.7 || return 1
    check policer 'steps_ok()' \
        'res("sweep", "down")["signature"] == "policer"' \
        'approx(res("sweep", "down")["capacity_mbps"], 30, 15)' \
        'res("sweep", "down")["max_queue_ms"] < 5' \
        'finding("rate.policer.down")'
}

scenario_shaper() {
    lab impair a2b --delay 10ms --rate 30mbit --buffer 40ms
    lab impair b2a --delay 10ms
    client shaper --tests sweep-down --max-rate 60 --scale 0.7 || return 1
    check shaper 'steps_ok()' \
        'res("sweep", "down")["signature"] == "shaper"' \
        'approx(res("sweep", "down")["capacity_mbps"], 30, 10)' \
        '20 < res("sweep", "down")["max_queue_ms"] < 80'
}

scenario_loss() {
    # 2% random loss downstream. Loss is asserted on a UDP stream of
    # thousands of packets (precise), not on the ~180 latency probes,
    # which can miss a 1% loss by chance.
    lab impair a2b --delay 10ms --loss 2%
    lab impair b2a --delay 10ms
    client loss --tests latency,tcp-down-1,tcp-down,udp-down,sweep-down --max-rate 100 --scale 0.6 || return 1
    check loss 'steps_ok()' \
        '1.4 < res("udp", "down")["loss"]["loss_pct"] < 2.6' \
        'res("udp", "down")["host_drops"] <= 0' \
        'finding("loss.idle") or finding("loss.udp.down")' \
        'res("sweep", "down")["signature"] == "non-congestive-loss"' \
        'finding("perflow.loss.down")' \
        'res("tcp", "down", 1)["retrans_pct"] > 0.5'
}

scenario_qos_fifo() {
    # A FIFO bottleneck with a deep buffer gives EF no protection.
    lab impair a2b --delay 5ms --rate 30mbit --buffer 200ms
    lab impair b2a --delay 5ms --rate 30mbit --buffer 200ms
    client qosfifo --tests qos --scale 0.6 || return 1
    check qosfifo 'steps_ok()' \
        'not res("qos")["priority_effective"]' \
        'res("qos")["classes"][2]["increase_ms"] > 50' \
        'finding("qos.ineffective")'
}

scenario_qos_prio() {
    # Strict priority for EF at the bottleneck: EF stays fast under load.
    lab impair a2b --delay 5ms --rate 30mbit --buffer 200ms --qdisc prio
    lab impair b2a --delay 5ms --rate 30mbit --buffer 200ms --qdisc prio
    client qosprio --tests qos --scale 0.6 || return 1
    check qosprio 'steps_ok()' \
        'res("qos")["priority_effective"]' \
        'res("qos")["classes"][0]["increase_ms"] < 0.5 * res("qos")["classes"][2]["increase_ms"]' \
        'finding("qos.priority")'
}

scenario_mix_fifo() {
    # A bloated FIFO, as on many cable and DSL links (a 600 ms upload
    # buffer): calls are fine on their own and fall apart once file
    # transfers fill the queue.
    lab impair a2b --delay 10ms --rate 20mbit --buffer 200ms
    lab impair b2a --delay 10ms --rate 5mbit --buffer 600ms
    client mixfifo --tests mix --mix "voice:4,video:1,web:1,download:1,upload:1" --scale 0.5 || return 1
    check mixfifo 'steps_ok()' \
        'mixkind("interactive", "voice")["verdict"] == "good"' \
        'mixkind("interactive", "video")["verdict"] in ("good", "acceptable")' \
        'mixkind("with-transfers", "voice")["verdict"] == "poor"' \
        'mixkind("with-transfers", "voice")["value"] < mixkind("interactive", "voice")["value"] - 1' \
        'mixkind("with-transfers", "video")["verdict"] == "poor"' \
        'res("mix")["phases"][1]["probe_rtt_ms"]["p95"] > 300' \
        'sev("mix.voice.degraded") == "warning" and finding("mix.video.degraded")'
}

scenario_mix_cake() {
    # The same link with CAKE: flow isolation keeps calls good while the
    # transfers run.
    lab impair a2b --delay 10ms --rate 20mbit --qdisc cake
    lab impair b2a --delay 10ms --rate 10mbit --qdisc cake
    client mixcake --tests mix --mix "voice:4,video:1,web:1,download:1,upload:1" --scale 0.5 || return 1
    check mixcake 'steps_ok()' \
        'mixkind("with-transfers", "voice")["verdict"] == "good"' \
        'mixkind("with-transfers", "video")["verdict"] in ("good", "acceptable")' \
        'mixkind("with-transfers", "web")["verdict"] in ("good", "acceptable")' \
        'not finding("mix.voice.degraded")' \
        'res("mix")["phases"][1]["probe_rtt_ms"]["p95"] < 60'
}

scenario_mix_prio() {
    # Strict priority for EF on a FIFO: marked voice is protected from the
    # transfers, video (AF41, not prioritised here) is not.
    lab impair a2b --delay 5ms --rate 20mbit --buffer 200ms --qdisc prio
    lab impair b2a --delay 5ms --rate 10mbit --buffer 200ms --qdisc prio
    client mixprio --tests mix --mix "voice:4,video:1,download:1,upload:1" --scale 0.5 || return 1
    check mixprio 'steps_ok()' \
        'res("mix")["dscp"]' \
        'mixkind("with-transfers", "voice")["verdict"] == "good"' \
        'mixkind("with-transfers", "video")["verdict"] == "poor"' \
        'not finding("mix.voice.degraded") and finding("mix.video.degraded")'
}

scenario_capacity() {
    # 4 Mbit/s each way with fq_codel: about 50 G.711 calls fit (80 kbit/s
    # each way with headers).
    lab impair a2b --delay 5ms --rate 4mbit --qdisc fq_codel
    lab impair b2a --delay 5ms --rate 4mbit --qdisc fq_codel
    client capacity --tests capacity-voice --scale 0.5 || return 1
    check capacity 'steps_ok()' \
        '30 <= res("capacity")["alone"]["acceptable"] <= 56' \
        'res("capacity")["alone"]["good"] <= res("capacity")["alone"]["acceptable"]' \
        'not res("capacity")["alone"].get("limit")' \
        '20 <= res("capacity")["with_transfers"]["acceptable"] <= 56' \
        'finding("capacity.voice")'
}

scenario_cdp() {
    # CDP replication over a 20 Mbit/s FIFO with a 200 ms buffer. Bursts
    # below the link rate keep a tight RPO; bursts above it fall behind and
    # fill the queue for everyone else.
    lab impair a2b --delay 10ms --rate 20mbit --buffer 200ms
    lab impair b2a --delay 10ms --rate 20mbit --buffer 200ms
    client cdpok --tests cdp-up --cdp-rate 4 --cdp-burst-secs 5 --cdp-period 15 --cdp-duration 30 || return 1
    check cdpok 'steps_ok()' \
        'res("cdp")["verdict"] == "good"' \
        'res("cdp")["max_lag_s"] < 1' \
        'approx(res("cdp")["throughput_mbps"], 4, 15)' \
        'finding("cdp.rpo.ok")' || return 1
    client cdpover --tests cdp-up --cdp-rate 12 --cdp-burst-secs 5 --cdp-period 15 --cdp-duration 30 --cdp-rpo 2 || return 1
    check cdpover 'steps_ok()' \
        'res("cdp")["verdict"] == "poor"' \
        'res("cdp")["backlogged"] and approx(res("cdp")["sustained_mbps"], 19, 15)' \
        'res("cdp")["max_lag_s"] > 2' \
        'finding("cdp.rpo.missed") and finding("cdp.latency")'
}

scenario_group() {
    # Latency, a voice call and CDP replication run on their own and then
    # together over a 20 Mbit/s FIFO: together, the replication bursts fill
    # the queue and the others get worse, which the comparison must show.
    lab impair a2b --delay 10ms --rate 20mbit --buffer 200ms
    lab impair b2a --delay 10ms --rate 20mbit --buffer 200ms
    client group --tests latency+voip+cdp-up --group-duration 20 --cdp-rate 12 --cdp-burst-secs 5 --cdp-period 10 || return 1
    check group 'steps_ok()' \
        'len(R["groups"]) == 1 and R["groups"][0]["baseline"]' \
        'len([s for s in R["steps"] if s.get("phase") == "together"]) == 3' \
        'any(c["test"] == "latency" and c["worse"] for c in R["load_comparisons"])' \
        'finding("group.g1.worse")'
}

scenario_ipv6() {
    # The same checks over IPv6, through two routers, with a 1400-byte
    # link so Packet Too Big handling is exercised.
    lab impair a2b --delay 5ms
    lab impair b2a --delay 5ms
    lab mtu 1400
    ./netns.sh exec b "$PG" test "[$A6]:7401" --key "$KEY" --no-resolve -q --json "$OUT/ipv6.json" \
        --tests info,latency,trace,tcp-down,tcp-up,udp-down,pmtu,integrity --scale 0.5 > "$OUT/ipv6.log" 2>&1 || { tail -5 "$OUT/ipv6.log"; return 1; }
    check ipv6 'steps_ok()' \
        'approx(res("latency")["udp_rtt_ms"]["median"], 10, 15)' \
        'len(res("trace", "up")["hops"]) == 3' \
        'res("tcp", "down")["mbps"] > 200 and res("tcp", "up")["mbps"] > 200' \
        'res("udp", "down")["loss"]["loss_pct"] < 1' \
        'res("pmtu")["up"]["mtu"] == 1400 and res("pmtu")["down"]["mtu"] == 1400' \
        'not res("pmtu")["up"]["blackhole"]' \
        'all(d["status"] == "preserved" for d in res("integrity")["dscp_up"] + res("integrity")["dscp_down"])' \
        'res("integrity")["hops_up"] == 2 and res("integrity")["hops_down"] == 2'
}

scenario_reorder() {
    lab impair a2b --delay 10ms
    lab impair b2a --delay 10ms --reorder 25%
    client reorder --tests udp-up --max-rate 20 --scale 0.5 || return 1
    check reorder 'steps_ok()' \
        'res("udp", "up")["loss"]["reorder_pct"] > 5' \
        'finding("reorder.up")'
}

scenario_middlebox() {
    lab impair a2b --delay 5ms
    lab impair b2a --delay 5ms
    lab mss-clamp 1200
    lab dscp-bleach on
    client middlebox --tests integrity || return 1
    check middlebox 'steps_ok()' \
        'res("integrity")["mss_clamped_up"] and res("integrity")["mss_clamped_down"]' \
        'res("integrity")["endpoint_snd_mss"] <= 1200' \
        'all(d["status"] == "bleached" for d in res("integrity")["dscp_up"] if d["sent"] != 0)' \
        'all(d["status"] == "bleached" for d in res("integrity")["dscp_down"] if d["sent"] != 0)' \
        'finding("mss.clamp") and finding("qos.bleached.up") and finding("qos.bleached.down")'
}

scenario_pmtu_blackhole() {
    lab mtu 1400
    lab icmp-block on
    client blackhole --tests pmtu || return 1
    check blackhole 'steps_ok()' \
        'abs(res("pmtu")["up"]["mtu"] - 1400) <= 4 and res("pmtu")["up"]["blackhole"]' \
        'abs(res("pmtu")["down"]["mtu"] - 1400) <= 4 and res("pmtu")["down"]["blackhole"]' \
        'sev("mtu.blackhole.up") == "critical"'
}

scenario_pmtu_ok() {
    lab mtu 1400
    client pmtuok --tests pmtu || return 1
    check pmtuok 'steps_ok()' \
        'abs(res("pmtu")["up"]["mtu"] - 1400) <= 4 and not res("pmtu")["up"]["blackhole"] and res("pmtu")["up"]["ptb_seen"]' \
        'abs(res("pmtu")["down"]["mtu"] - 1400) <= 4 and not res("pmtu")["down"]["blackhole"]' \
        'finding("mtu.reduced") and not finding("mtu.blackhole")'
}

scenario_nat() {
    lab impair a2b --delay 5ms
    lab impair b2a --delay 5ms
    lab nat on
    client nat --tests info,latency,trace,tcp-down,tcp-up,udp-down,udp-up,pmtu,integrity,voip --scale 0.5 || return 1
    check nat 'steps_ok()' \
        'res("integrity")["nat"] and res("integrity")["client_observed"].startswith("10.77.0.2:")' \
        'finding("nat.detected")' \
        'res("udp", "down")["mbps"] > 0 and res("udp", "down")["loss"]["loss_pct"] < 1' \
        'res("pmtu")["down"]["mtu"] == 1500' \
        'len(res("trace", "down")["hops"]) >= 1' \
        'res("voip")["down"]["mos"] > 4'
}

scenario_owd() {
    lab impair a2b --delay 30ms
    lab impair b2a --delay 5ms
    client owd --tests info,latency --scale 0.5 || return 1
    check owd 'steps_ok()' \
        'approx(res("latency")["udp_rtt_ms"]["median"], 35, 10)' \
        '(not res("info")["owd_trustworthy"]) or (approx(res("latency")["owd_down_ms"]["median"], 30, 10) and approx(res("latency")["owd_up_ms"]["median"], 5, 40))'
}

scenario_sqm() {
    lab impair a2b --delay 10ms --rate 50mbit --buffer 300ms
    lab impair b2a --delay 10ms --rate 10mbit --buffer 500ms
    client sqm --tests tcp-down,tcp-up,sqm --allow-shaping || return 1
    local leftover
    leftover=$(./netns.sh exec b tc qdisc show dev b0 | grep -c "5047:" || true)
    check sqm 'steps_ok()' \
        'grade_rank(res("sqm")["baseline_grade"]) >= 3' \
        'res("sqm")["recommended"] is not None and grade_rank(res("sqm")["recommended"]["grade"]) <= 1' \
        'finding("sqm.recommended")' \
        "$leftover == 0"
}

scenario_unprivileged() {
    lab impair a2b --delay 5ms
    lab impair b2a --delay 5ms
    ./netns.sh exec b setpriv --reuid=65534 --regid=65534 --clear-groups "$PG" test "$A" --key "$KEY" --no-resolve -q \
        --json /tmp/pg-unpriv.json --tests latency,trace,tcp-down,pmtu,integrity --scale 0.5 > "$OUT/unpriv.log" 2>&1 || { tail -5 "$OUT/unpriv.log"; return 1; }
    cp /tmp/pg-unpriv.json "$OUT/unpriv.json"
    check unpriv 'steps_ok()' \
        'res("trace", "up")["method"] == "udp" and len(res("trace", "up")["hops"]) == 3' \
        'res("tcp", "down")["mbps"] > 100' \
        'res("pmtu")["up"]["mtu"] == 1500' \
        'res("latency").get("icmp_note") or res("latency").get("icmp_rtt_ms")'
}

# --- main ------------------------------------------------------------------

./netns.sh down >/dev/null 2>&1 || true
./netns.sh up >/dev/null
trap './netns.sh down >/dev/null 2>&1' EXIT
./netns.sh exec a "$PG" endpoint --listen "$A:7400" --key "$KEY" --log-level warn > "$OUT/endpoint.log" 2>&1 &
./netns.sh exec a "$PG" endpoint --listen "[$A6]:7401" --key "$KEY" --log-level warn > "$OUT/endpoint6.log" 2>&1 &
sleep 1

ALL=(clean bufferbloat cake policer shaper loss reorder middlebox pmtu_blackhole pmtu_ok nat owd sqm unprivileged ipv6 qos_fifo qos_prio mix_fifo mix_cake mix_prio capacity cdp group)
SEL=("$@")
[ ${#SEL[@]} -eq 0 ] && SEL=("${ALL[@]}")
for s in "${SEL[@]}"; do run "$s"; done

echo
echo "passed: ${#PASSED[@]} (${PASSED[*]:-})"
echo "failed: ${#FAILED[@]} (${FAILED[*]:-})"
[ ${#FAILED[@]} -eq 0 ]
