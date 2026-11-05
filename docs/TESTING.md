# How PathGauge is tested

PathGauge is tested in layers. The aim is that measurements are proven correct against known conditions, rather than only shown to run.

| Layer | What it proves | Run with |
|---|---|---|
| Unit tests | Maths and logic: percentiles, RFC 3550 jitter, loss/reorder/duplicate/burst tracking, MOS, grades, the Mathis bound, the wire protocol and MACs, config and policy validation, `doctor` against simulated Rocky 8 / Ubuntu hosts, the diagnosis rules (from fixtures), sweep classification, PMTU bisection, DSCP/ECN classification, traceroute loss interpretation, the pacer (including a regression test for a slow sender), password hashing, alert evaluation, monitoring rollups | `make test` |
| Loopback integration | A real endpoint and client in one process: latency, TCP both directions with live intervals and sender `TCP_INFO`, stream scaling, wrong-key rejection | `go test ./internal/engine/` |
| Server + agent integration | An in-process server and agent over real TLS and WebSockets (detailed below) | `go test ./internal/server/` |
| Network-namespace lab | 23 scenarios, each building a known fault with tc, nftables and netem, then checking PathGauge's measurements **and** diagnosis (see below) | `make lab-scenarios` |
| Packages | Install, reinstall (configuration kept), remove, ownership and permissions, `systemd-analyze verify` on Ubuntu 22.04/24.04/26.04 and Rocky 8/9/10 | `make install-test` |
| Packaged end to end | The RPM installed in systemd containers, a **Rocky 9 server and a Rocky 8 agent**, put through the full server/agent workflow (detailed below) | `make e2e` |
| Real world | Peer and server modes over the internet across three continents (client behind NAT, on Wi-Fi), including the SQM tuner shaping a live VPS | manual |
| Race detector | The whole suite under `go test -race` (in the official Go container, since it needs cgo): no races in the endpoint, engine, hub or agent | `docker run … golang go test -race ./...` |
| Dashboard | Strict TypeScript build; Playwright smoke test touring every page, making changes, watching a live run, plus a read-only viewer pass | `cd web && npm run build && npm run smoke` |
| Static checks | golangci-lint (errcheck, gosec, staticcheck, errorlint and others), go vet for amd64 and arm64, shellcheck, actionlint, dependency licence audit | `make lint`, `make licences` |

The server + agent integration test covers:

- the setup flow and rejection of weak passwords
- CSRF protection
- join tokens, including a wrong CA pin and token reuse
- agent enrolment over mutual TLS
- a run started through the API, its report, the HTML report and a share link
- overview and Prometheus metrics
- validation of alert rules and schedules
- revocation

The packaged end-to-end test runs, in order:

1. `server init --enable`
2. `join-token`
3. `agent join --enable`
4. a 13-step run through the REST API, including Paris traceroute (proves `CAP_NET_RAW`) and the SQM tuner shaping from inside the systemd sandbox (proves `CAP_NET_ADMIN` and tc)
5. a check that no qdisc or IFB device is left behind

## Lab scenarios

The lab (`lab/netns.sh`) has two endpoints with two routers between them. Delay, jitter and loss are applied on the router-to-router link, and the bottleneck on each router's link towards the receiver, so the endpoints' own TCP stacks are never distorted. Each scenario asserts specific values:

| Scenario | Fault built | Main checks |
|---|---|---|
| clean | 5 ms each way, 150 Mbit/s fq_codel bottleneck | RTT 10 ms ±15%, 3 hops both ways, path MTU 1500, every DSCP and ECN mark preserved, no NAT, 2 hops from TTL, no warnings |
| bufferbloat | 50/10 Mbit/s tbf, 300/500 ms FIFO buffers | Download 47.8 Mbit/s ±6% **and within 5% of iperf3**, upload 9.5 ±8%, grades ≥ C and ≥ D, FIFO queue, sweep signature "bufferbloat" |
| cake | the same rates with CAKE | Grade A or better, flow-queued, "latency stays low" finding |
| policer | 30 Mbit/s policer | Sweep signature "policer", capacity 30 ±15%, queue < 5 ms |
| shaper | 30 Mbit/s tbf, 40 ms buffer | Signature "shaper", queue 20–80 ms |
| loss | 2% random loss | UDP loss 2% ±0.6 with no host drops, "non-congestive loss" signature, single stream diagnosed as loss-limited |
| reorder | 25% reordering upstream | Reordering > 5% and a finding |
| middlebox | MSS clamp to 1200, DSCP bleached | MSS clamped both ways, every mark bleached both ways |
| pmtu_blackhole | 1400-byte link, ICMP "fragmentation needed" dropped | Path MTU 1400 each way, black hole flagged critical |
| pmtu_ok | 1400-byte link | MTU 1400, "packet too big" seen, reduced-MTU finding, no black hole |
| nat | the client masqueraded behind a stateful NAT | Every test works, including reverse UDP, reverse PMTU and the return trace; NAT detected |
| owd | 30 ms down, 5 ms up | RTT 35 ms; one-way delays 30 and 5 ms when clocks are synced |
| sqm | the bufferbloat path, plus `--allow-shaping` | Baseline ≥ D, the tuner finds a setting with grade A or better, no qdisc left behind |
| unprivileged | runs as `nobody` | Fallback traceroute, TCP, PMTU and ICMP handling without privileges |
| qos_fifo | 30 Mbit/s FIFO with a 200 ms buffer | EF latency rises like best effort: priority "ineffective" |
| qos_prio | The same, with strict priority for EF | EF stays fast while best effort queues: priority "effective" |
| mix_fifo | 20/5 Mbit/s FIFO with 200/600 ms buffers; mix of 4 voice calls, a video call, a web user and a download and upload | Calls good without transfers; voice and video poor with them (MOS drops by more than 1); "degraded" findings |
| mix_cake | the same rates with CAKE | Calls stay good while transfers run; browsing good; no voice "degraded" finding |
| mix_prio | 20/10 Mbit/s FIFO with strict priority for EF | Marked voice stays good under transfers; video (AF41, not prioritised) is poor |
| capacity | 4 Mbit/s each way with fq_codel | Voice capacity 30–56 calls (about 50 fit), 20–56 while transfers run |
| cdp | 20 Mbit/s FIFO with a 200 ms buffer; CDP replication at 4 and then 12 Mbit/s average (bursts of 12 and 36 Mbit/s) | Bursts under the link rate: lag under 1 s, "good", throughput 4 Mbit/s ±15%. Bursts over it: lag over a 2 s RPO, "poor", the path carried ~19 Mbit/s while changes waited, RPO and latency findings |
| group | 20 Mbit/s FIFO; latency, a voice call and CDP replication alone and then together | Three "together" steps; latency marked worse together (round trip p95 20 → ~215 ms); a "group got worse" finding |
| ipv6 | IPv6 through both routers, 1400-byte link | RTT, 3 hops, TCP and UDP, path MTU 1400 each way from Packet Too Big, DSCP preserved through the traffic class, hop limits |
