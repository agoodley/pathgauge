<img src="assets/icon.svg" width="64" height="64" alt="">

# PathGauge

Two-ended network diagnostics for Linux. Install PathGauge on two machines and it measures the path between them, then explains what it finds:

- **Throughput:** single and parallel TCP streams in each direction, with the sender's own TCP state (RTT, cwnd, retransmissions, whether the receive window or send buffer was the limit), and paced UDP.
- **Latency, jitter and loss:** idle round trip, one-way delay when clocks are synchronised, ICMP vs UDP vs TCP-connect comparison, loss bursts, reordering.
- **Bufferbloat:** latency under load in each direction, with an A+…F grade, responsiveness in RPM, queue type (FIFO or flow-queued) and estimated buffer size.
- **Rate sweeps:** the network's response curve from low load to beyond capacity, classifying the bottleneck as a policer, shaper, bufferbloat, burst allowance or non-congestive loss. Also packet-size sweeps (packets per second limits) and stream-count scaling.
- **Path:** traceroute and MTR in **both directions at once** (Paris-style when privileged, with MPLS labels), with loss correctly separated from ICMP rate limiting.
- **Middleboxes:** path MTU and black holes in each direction, MSS clamping, stripped TCP options, DSCP and ECN preservation, NAT and carrier-grade NAT.
- **Traffic mix:** simulated voice and video calls, gaming, web browsing, video streaming and file transfers running at once, each on its own flow and graded the way users experience it (MOS, frames on time and freezes, page-load and chunk times), with and without bulk transfers. A **call-capacity** search finds how many simultaneous calls the path carries, on their own and while transfers run.
- **Applications:** a simulated voice call with MOS (idle and under load), QoS priority under load (EF vs best effort), short-transfer times, comparison of congestion-control algorithms, UDP throttling and WAN-optimiser checks.
- **Traffic shaping:** time-limited shaping with CAKE, fq_codel, tbf or netem, plus an **SQM tuner** that finds the shaping rate that cures bufferbloat.
- **Diagnosis:** findings with evidence and recommendations, a 0–100 health score, and a one-page link fingerprint.
- **Server, agents and dashboard:** enrol agents with single-use tokens over mutual TLS; run tests from the dashboard; continuous monitoring with hour × weekday heatmaps; schedules; alerts by webhook, Slack, ntfy or e-mail; Prometheus metrics; technical and executive HTML reports with share links.

The client side always opens the connections, so **clients and agents can sit behind NAT**. Only the endpoint or server needs open ports.

> **Status: pre-release.** Release candidates are on the [releases page](https://github.com/agoodley/pathgauge/releases). Expect changes before 1.0.

## Responsible use

Only test networks you are authorised to test. Throughput tests and sweeps deliberately fill the link and use a lot of data (roughly 5 GB for a quick run and 10–15 GB for a standard run at 1 Gbit/s). Shaping changes how an interface queues **all** of its traffic while a test runs.

## Quick start

### Two boxes, no server

```bash
# box A (reachable on 7400/tcp and 7400/udp)
pathgauge endpoint --key-file ~/.config/pathgauge/key
# box B (may be behind NAT)
pathgauge test boxA.example.com --key pg_... --profile standard --html report.html
```

### Server and agents

```bash
# server
sudo pathgauge server init --enable          # then open https://SERVER:7443
sudo pathgauge server join-token
# each agent
sudo pathgauge agent join https://SERVER:7443 --token pgjoin_... --name branch --enable
```

The [user guide](docs/USER-GUIDE.md) covers installation, ports, every test, how to read the results, shaping safety, monitoring and alerts, and troubleshooting.

## Install

Packages are built for Rocky Linux / RHEL / AlmaLinux 8, 9 and 10, and Ubuntu 22.04, 24.04 and 26.04, on x86_64 and aarch64. Both services run as the unprivileged `pathgauge` user with only `CAP_NET_RAW` and `CAP_NET_ADMIN`.

```bash
make snapshot        # builds the dashboard, then .rpm/.deb/.tar.gz into dist/ (needs Go, Node and GoReleaser)
sudo dnf install ./dist/pathgauge-*.x86_64.rpm      # or: sudo apt install ./dist/pathgauge_*_amd64.deb
sudo pathgauge doctor
```

## Development and testing

| Task | Command |
|---|---|
| Unit and integration tests (including in-process server + agent) | `make test` |
| Lint and licence check | `make lint`, `make licences` |
| Dashboard build (`web/` → embedded) | `make ui` |
| Local server + agent with demo data | `make dev-stack` |
| Packages | `make snapshot` |
| Install/upgrade/remove on 6 distros (Docker) | `make install-test` |
| Packaged Rocky 9 server + Rocky 8 agent under systemd (Docker) | `make e2e` |
| Accuracy: 21 known-fault scenarios in a network-namespace lab (Docker) | `make lab-scenarios` |

See [docs/TESTING.md](docs/TESTING.md) for what each layer covers and the latest results. [docs/PLAN.md](docs/PLAN.md) has the design and roadmap, and [docs/API.md](docs/API.md) the REST API.

## Licence

[Apache License 2.0](LICENSE). See also [NOTICE](NOTICE).

---

Created in [T3 Code](https://t3.codes).
