# PathGauge: Product & Technical Plan

**Status:** Draft v2 (scope decisions recorded in §2) · **Date:** 2026-11-05 · **Owner:** Andy Goodley

---

## Contents

1. [Summary](#1-summary)
2. [Goals, non-goals, assumptions](#2-goals-non-goals-assumptions)
3. [Concepts](#3-concepts)
4. [Architecture](#4-architecture)
5. [Technology choices](#5-technology-choices)
6. [Installation & deployment](#6-installation--deployment)
7. [Test catalogue](#7-test-catalogue)
8. [Rate sweeps & traffic shaping](#8-rate-sweeps--traffic-shaping)
9. [Test orchestration & profiles](#9-test-orchestration--profiles)
10. [Diagnosis engine](#10-diagnosis-engine)
11. [Dashboard](#11-dashboard)
12. [Reports](#12-reports)
13. [Monitoring, scheduling & alerting](#13-monitoring-scheduling--alerting)
14. [API, CLI & integrations](#14-api-cli--integrations)
15. [Security](#15-security)
16. [Data model & storage](#16-data-model--storage)
17. [Protocols](#17-protocols)
18. [Repository layout](#18-repository-layout)
19. [Quality strategy](#19-quality-strategy)
20. [Non-functional requirements](#20-non-functional-requirements)
21. [Roadmap](#21-roadmap)
22. [Risks & mitigations](#22-risks--mitigations)
23. [Open questions](#23-open-questions)
24. [Open-source readiness](#24-open-source-readiness)

---

## 1. Summary

PathGauge is a self-hosted network diagnostics platform for Linux that tests the path between two machines. You install the **server** on one box (Rocky Linux or Ubuntu) and the **agent** on another. The two ends then run a coordinated set of tests across the path between them:

- throughput over single and parallel streams
- latency, jitter and loss
- hops
- bufferbloat
- MTU
- middlebox behaviour
- QoS

It also runs **rate sweeps and traffic-shaping experiments**, which show how the path behaves across its whole operating range.

A web dashboard shows live and historical results. A diagnosis engine turns the measurements into plain-English findings, and a report generator produces PDF or HTML reports for management, for engineers, or for an ISP escalation.

Single-ended tools (speed-test sites, ping, mtr) control only one end of the path. PathGauge controls **both ends**, so it can measure things they can't:

- one-way metrics
- the forward path and the reverse path
- what middleboxes change in transit (MSS, DSCP, ECN, TCP options, NAT)
- the point in the operating range where the network starts to queue or drop

## 2. Goals, non-goals, assumptions

### Goals

1. Install in minutes from native packages on Rocky 8/9/10 and Ubuntu 22.04/24.04/26.04, on x86_64 and aarch64.
2. Need only two boxes at minimum, and scale to many agents and agent-to-agent mesh testing later.
3. Provide a comprehensive and accurate test suite, including standards-based modes (RFC 2544, Y.1564, RFC 6349 styles) whose results carriers will take seriously.
4. **Explain** results instead of only plotting them. Every finding carries evidence, a confidence level and a recommendation.
5. **Be safe:**
   - never leave a network shaped or broken
   - never become an open traffic reflector
   - keep data usage within limits
   - account for every disruptive action
6. Work fully offline and in air-gapped networks.

### Non-goals (v1)

- Windows or macOS agents. The engine stays portable, but Linux comes first.
- A public speed-test service, or testing against third-party servers. An optional "external targets" module may follow later.
- Passive monitoring of production traffic (NetFlow, sFlow, SNMP polling). PathGauge does active testing only.
- Multi-tenant SaaS.

### Scope decisions (2026-11-05)

| Question | Decision | Effect on the plan |
|---|---|---|
| Link speeds | **10 Gbps maximum; 1 Gbps typical** | The engine must reach 10 Gbps line rate (§20), but defaults, data estimates and the Phase 1 acceptance tests are tuned for 1 Gbps. Nothing above 10 Gbps is in scope, so the optional iperf3 backend is dropped. |
| Scale | **Two boxes needed now; will grow** | The roadmap delivers a usable two-box tool as early as possible (the Phase 1 peer CLI). SQLite stays the default. The data model and control protocol stay multi-agent from day one, so the agent-to-agent mesh can be added later without migrations. |
| Audience | **Likely published on GitHub as open source** | New §24 (open-source readiness). Every dependency must have a compatible licence, nothing proprietary is bundled, and no secrets, internal hostnames or telemetry go in the repo. CI, releases and package repositories live on GitHub. |
| Shaping scope | **Shaping the whole interface is acceptable for now** | Whole-interface shaping becomes the default scope (§8.3), which is simpler to build. Test-traffic-only shaping moves to Phase 7, ahead of wider public use. |
| Licence | **Apache-2.0** | LICENSE and NOTICE are in the repo, packages declare it, and CI fails on any dependency with an incompatible licence (§24). |
| First two boxes | **Rocky 8 and Rocky 9, neither behind NAT** | These are the Phase 1 acceptance targets. Rocky 8 can't run CAKE (its kernel doesn't ship it) and needs `kernel-modules-extra` for netem, so the SQM tuner falls back to fq_codel there, and `doctor` says so. |
| NAT | **The client will soon sit behind NAT; the server stays reachable** | Phase 1 must work with the client behind NAT (see Phase 1). The lab's `nat on` mode reproduces this. A restricted mode that only allows port 443 is not needed yet. |

### Assumptions

These shape the design. §23 lists the questions that are still open.

- The agent may sit behind NAT or a firewall. The server is reachable from the agent on one TCP port and one UDP port.
- Installers have root. The services themselves run as a non-root user with specific Linux capabilities.

## 3. Concepts

| Term | Meaning |
|---|---|
| **Server** | Box A. Runs the dashboard, API, database, scheduler, analysis and reports. It includes a **built-in local agent**, so it is also a test endpoint. That keeps the minimum deployment at exactly two boxes. |
| **Agent** | Box B, plus any further boxes. Runs tests when told to and streams results back. |
| **Endpoint** | Anything that can send or receive test traffic: the server's local agent or a remote agent. |
| **Link** | A named pair of endpoints, for example "HQ ↔ Leeds". A link holds its expectations (contracted speeds, SLA targets). Its directions are **A→B** and **B→A**; the UI also shows friendly labels such as "HQ → Leeds". |
| **Test** | One type of measurement, for example `throughput.tcp`. |
| **Profile** | An ordered suite of tests with parameters, for example "Standard". |
| **Run** | One execution of a profile on a link. A run is made up of **steps**. |
| **Finding** | A diagnosis produced by the analysis engine, with severity, confidence, evidence and a recommendation. |
| **Lease** | A time-limited permission, for example a shaping configuration that removes itself automatically when the lease expires. |

## 4. Architecture

```
                    Browser (dashboard)
                          │  HTTPS :7443  (REST + WebSocket)
                          ▼
┌───────────────────────── Box A: pathgauge server ─────────────────────────┐
│  Web UI (embedded SPA) · REST API · Auth/RBAC · Scheduler · Alerting        │
│  Orchestrator · Analysis/Findings · Report renderer · Exporters (Prom/OTel) │
│  Store: SQLite (default) / PostgreSQL                                        │
│  Built-in local agent ── Test engine ── Data-plane endpoint :7400 tcp/udp   │
└──────────────▲────────────────────────────────────────────▲─────────────────┘
               │ gRPC over mTLS, agent dials out             │ test traffic
               │ (jobs, live interval results, host facts)   │ TCP / UDP / ICMP
┌──────────────┴───────────────── Box B: pathgauge agent ──┴─────────────────┐
│  Control client · Test engine · Data-plane endpoint · Shaper (tc + leases)   │
│  Host diagnostics · Local policy & safety limits · Offline result spool      │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Key design decisions

1. **A single static Go binary** named `pathgauge`, with subcommands `server`, `agent`, `endpoint`, `test`, `run`, `report`, `tune`, `doctor` and others.
   - One package covers both roles, and you pick the role at init.
   - It has no runtime dependencies beyond iproute2, which is present everywhere.
2. **The control plane is separate from the data plane.**
   - The control plane is gRPC over mTLS, on the same port as the HTTPS UI (7443, split by ALPN).
   - Test traffic uses its own port (7400 tcp/udp, configurable) plus ICMP, so control chatter never pollutes measurements.
3. **NAT-friendly: the agent starts every connection.**
   - The agent dials the control channel.
   - For B→A tests, the agent sends the traffic.
   - For A→B tests, the agent opens the connection and the server sends (reverse mode).
   - For UDP, the agent sends the first packet to open the NAT binding.
   - As a result, only the server needs inbound ports.
4. **One test engine everywhere.** The same engine runs in the server's local agent, in remote agents and in the standalone CLI peer mode. Tests are plugins, each described by a typed spec (§9).
5. **Agents are autonomous and enforce safety locally.**
   - Each agent enforces a local policy that the server can't override (maximum rate, allowed tests, allowed hours, whether shaping is allowed).
   - If the control channel drops, the agent spools results and keeps running continuous probes, then uploads when it reconnects.
6. **The engine works without a server.**
   - `pathgauge endpoint` on one box and `pathgauge test <host>` on the other run the full suite peer-to-peer, much like iperf3.
   - The results can be imported into a server later.

## 5. Technology choices

| Area | Choice | Why | Alternatives considered |
|---|---|---|---|
| Server and agent | **Go** (current stable) | Static binaries, trivial arm64 cross-compiles, a strong net stack, `x/sys/unix` for `TCP_INFO`, `SO_MAX_PACING_RATE`, `IP_RECVERR` and raw sockets, and cheap goroutines for parallel streams | Rust (faster, but a slower team velocity); Python (performance and packaging pain) |
| Agent control RPC | **gRPC bidirectional streaming + protobuf** | Typed contract, streams live results, native mTLS | WebSocket + JSON |
| Public API | **REST/JSON + OpenAPI 3**, WebSocket for live data | curl-friendly, easy to script, generated docs | GraphQL |
| Database | **SQLite** (WAL, `modernc.org/sqlite`, pure Go, so no cgo) by default; **PostgreSQL** (+ optional TimescaleDB) for large fleets | Zero configuration, backup is one file | Influx/Prometheus as the primary store (rejected; offered as exporters instead) |
| Data access | `sqlc` + `goose` migrations | Typed queries, works with both databases | ORM |
| Frontend | **React + TypeScript + Vite**, TanStack Query/Router, Tailwind + shadcn/ui, embedded in the binary via `go:embed` | Mature ecosystem, nothing extra to install | Svelte, htmx |
| Dashboard charts | **Apache ECharts** | Handles series of 100k+ points, heatmaps, box plots, graph/topology layouts and optional maps | uPlot (fast, but limited chart types), Chart.js |
| Report charts | A Go SVG chart renderer | One set of chart code feeds both the HTML and the PDF reports, and it works without a browser | Server-side ECharts (needs Node) |
| PDF | **Typst**, shipped in an optional `pathgauge-pdf` package | Small, fast, deterministic, offline, good typography. You can also always use the browser's "Print to PDF" on the HTML report. | Headless Chromium (around 300 MB), wkhtmltopdf (abandoned) |
| Shaping | iproute2 `tc` called **without a shell**, plus netlink for reading state | Covers every qdisc, including CAKE. The exact commands are shown to the user. | Pure netlink (incomplete CAKE support) |
| Raw networking | `golang.org/x/net/{icmp,ipv4,ipv6}`, `x/sys/unix`, AF_PACKET for optional capture | No cgo or libpcap needed | gopacket + libpcap |
| Enrichment | IPinfo Lite / DB-IP Lite MMDB (ASN, country), optional MaxMind GeoLite2 with the user's own key, PeeringDB for IXPs, rDNS. The databases are **downloaded** by `pathgauge enrich update` or shipped in the offline bundle, never committed to the repo (§24). | Offline lookups with permissive licences | Live whois (slow, rate-limited) |
| Scheduling | `robfig/cron` + in-house job queue | Simple | — |
| Packaging | **GoReleaser + nfpm** → signed `.rpm`/`.deb`, YUM/APT repositories, `install.sh` | One pipeline produces packages for both distro families | Native spec/debian files |
| Observability | `slog` to journald; Prometheus `/metrics` for the app itself | Standard | — |

## 6. Installation & deployment

### Support matrix

| Distro | Versions | Arch | Notes |
|---|---|---|---|
| Rocky Linux (and RHEL/Alma) | 8, 9, 10 | x86_64, aarch64 | Rocky 8's 4.18 kernel lacks some features. Runtime detection disables the affected tests. |
| Ubuntu | 22.04, 24.04, 26.04 LTS | amd64, arm64 | |

### Server install

```bash
# Option A: repository (detects dnf/apt, adds a signed repo, installs)
curl -fsSL https://<release-host>/install.sh | sudo bash

# Option B: package file
sudo dnf install ./pathgauge-1.0.0-1.x86_64.rpm      # Rocky
sudo apt install ./pathgauge_1.0.0_amd64.deb         # Ubuntu

# Initialise the server role (interactive or with flags)
sudo pathgauge server init
#  - creates the admin user
#  - TLS: self-signed (generated), ACME/Let's Encrypt, or your own cert
#  - ports, data directory, retention
#  - offers to open firewalld/ufw rules
#  - enables + starts pathgauge-server.service
# → https://box-a:7443
```

### Agent install

```bash
sudo dnf install pathgauge     # or: sudo apt install pathgauge
sudo pathgauge agent join https://box-a:7443 --token 7F3K-Q9PL-XXXX --name leeds-branch
```

The join flow works like this:

1. An admin creates a **single-use, expiring enrollment token** in the UI.
2. The agent generates a keypair and sends a CSR together with its host facts.
3. The server checks the token, signs the certificate with its **internal CA** and returns the CA certificate, which the agent pins.
4. From then on the agent connects with mTLS.
5. Optionally, an admin must approve the agent before it can run tests.

### Package contents

- `/usr/bin/pathgauge`, with bash/zsh completion and man pages.
- Two systemd units: `pathgauge-server.service` and `pathgauge-agent.service`.
- Configuration in `/etc/pathgauge/{server,agent}.yaml`, and the agent's local policy in `/etc/pathgauge/policy.yaml`.
- Data in `/var/lib/pathgauge/`; logs go to journald.
- A system user `pathgauge`.

### Privileges

Neither service runs as root. Each runs as the `pathgauge` user with these systemd ambient capabilities:

| Capability | Why |
|---|---|
| `CAP_NET_RAW` | ICMP probes, TCP-SYN traceroute, optional packet capture |
| `CAP_NET_ADMIN` | `tc` shaping, `SO_MARK`, `SO_RCVBUFFORCE`/`SO_SNDBUFFORCE` for large test buffers |
| `CAP_NET_BIND_SERVICE` | **Only when** port-discrimination tests on ports 80/443 are enabled |

The units also apply systemd hardening: `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `NoNewPrivileges`, `RestrictAddressFamilies`, `SystemCallFilter`.

### Ports

| Port | Where | Purpose |
|---|---|---|
| 7443/tcp | Server | HTTPS UI, REST API, agent gRPC control |
| 7400/tcp + 7400/udp | Server (and an agent, when it must accept agent-to-agent tests) | Test data plane |
| ICMP | Both | Ping and traceroute replies, PMTU messages |
| Optional extra ports | Server | Port-discrimination tests (for example 80, 443, a random high port) |
| 443/tcp + 443/udp (restricted mode) | Server | Data plane for networks that only allow 443 |

### Operating-system integration

- **Firewall:** `server init` offers to add firewalld (Rocky) or ufw (Ubuntu) rules. If you decline, it prints the exact commands instead.
- **SELinux:** The binary runs as `unconfined_service_t` under the default targeted policy, so it works out of the box. Phase 7 adds a dedicated policy module and port labels.
- **Kernel capability detection:** Each endpoint reports a capability matrix covering BBR, CAKE, fq, IFB, UDP GSO/GRO, TCP_INFO chrono stats, ping sockets and IPv6. The UI greys out any test whose requirements are missing and explains why.
- **Host tuning:** `pathgauge tune --check` reports sysctl changes needed for the measured BDP (rmem/wmem maximums, fq qdisc, MTU probing). `--apply` makes those changes reversibly, and only when asked.

### Lifecycle

- **Upgrades:** Through the package manager. The server shows version skew, and agents stay protocol-compatible with the N-1 version. Optionally, the server can push signed agent upgrades.
- **Uninstall:** Removes the units and keeps data. `purge` / `dnf remove` plus a cleanup script removes the data too.
- **Air-gapped:** Offline package bundle plus an offline enrichment-database bundle.
- **Containers:** A Docker image (`--network host`) for labs and demos only. The UI warns that container networking skews results.

## 7. Test catalogue

Every test result stores:

- its parameters
- the software version
- a host-diagnostics snapshot from both ends
- the clock-sync state

This makes every result reproducible and auditable. Tests can run **A→B, B→A or in both directions at once**, over **IPv4, IPv6 or both** (a dual-stack comparison).

### 7.1 Baseline connectivity & latency

| Test ID | What it does | Key outputs |
|---|---|---|
| `reach` | ICMP echo, TCP connect and UDP echo to the endpoint, plus control-channel RTT | Which protocols pass, and basic up/down state |
| `latency.idle` | UDP probes (default 20 pps for 30 s) carrying four timestamps (STAMP-style), with ICMP and TCP-connect probes in parallel | Min/median/mean/p95/p99/max RTT; jitter (RFC 3550 estimator, standard deviation, IPDV/PDV per RFC 5481); loss; reordering; duplicates. When clocks are synced: **one-way delay** and OWD variation per direction. |
| `latency.compare` | Compares ICMP, UDP and TCP RTT | Shows whether ICMP is deprioritised (a common source of misleading ping results) |
| `clock` | Reads sync state via `adjtimex` and compares an NTP-style offset estimate between the two endpoints | Sync state and estimated error. This decides whether OWD figures are shown or suppressed. |

### 7.2 Throughput

| Test ID | What it does | Key outputs |
|---|---|---|
| `throughput.tcp` | Bulk TCP with configurable streams (1–128), duration, direction, congestion control, buffers, MSS and DSCP. The payload is random and incompressible. Samples are taken every 100 ms, and `TCP_INFO` is polled on each stream. | Goodput per interval and per stream; retransmits; RTT/rttvar under load; cwnd; pacing and delivery rate; **% of time rwnd-limited, sndbuf-limited or busy** (Linux ≥ 4.10), which tells you whether the receiver window, the sender buffer or the network is the limit; stream fairness (Jain index); time to reach 90% of steady state |
| `throughput.udp` | Paced UDP at a target rate (or "max"), with sequence numbers, using `sendmmsg` and UDP GSO | Delivered rate, loss, loss burstiness (Gilbert-Elliott model), jitter, reordering, duplication, OWD |
| `throughput.streams` | **Single vs parallel**: 1, 2, 4, 8, 16 and 32 streams in each direction | Scaling curve, and **single-stream efficiency** (1-stream rate ÷ best rate), which diagnoses per-flow limits |
| `throughput.cc` | Compares congestion-control algorithms (cubic, bbr, reno, or whatever the kernel offers) | Throughput, retransmits and RTT inflation for each algorithm, which shows how loss-sensitive the path is |
| `throughput.window` | Sweeps socket buffer size from 64 KB to 64 MB | Throughput against window size, which checks the BDP calculation |
| `throughput.shortflow` | Many short transfers (10 KB, 100 KB, 1 MB) including TCP connect and optional TLS handshake | Distribution of flow completion times, connect time and handshake time, which is what web and app users actually feel |
| `throughput.discrimination` | Repeats the same test on different ports (80, 443, 7400, a random high port) and protocols (TCP, UDP, TLS-wrapped, optionally QUIC) | Detects port or protocol throttling, DPI-based shaping and UDP throttling |
| `throughput.compressibility` | Compares a random payload with a highly compressible one | Detects WAN optimisers or compression inflating results |

### 7.3 Bufferbloat & responsiveness (`bufferbloat`)

**Procedure.** The test runs four phases:

1. idle (10 s)
2. A→B saturated (15 s)
3. B→A saturated (15 s)
4. bidirectional (15 s)

Latency is sampled continuously throughout, in three ways:

- **(a)** on a separate UDP flow at 20–50 Hz
- **(b)** in-flow, from the loaded TCP connections' `TCP_INFO` RTT
- **(c)** as new-connection setup time

**Outputs:**

- Latency increase per phase (median, p95, p99).
- A **grade from A+ to F** based on the p95 increase. The defaults are <5, <30, <60, <200, <400 and ≥400 ms, and they are configurable.
- **Responsiveness in RPM** (round-trips per minute), following the IETF IPPM responsiveness method.
- **Separate-flow vs in-flow comparison:**
  - If the separate-flow latency stays low while in-flow latency grows, flow queueing (fq_codel/CAKE) is present.
  - If both grow, there is a single shared FIFO buffer.
- **Estimated buffer size**, in milliseconds and in bytes (queue delay × bottleneck rate).
- Which direction is bloated.

**Recommendation.** Enable SQM (CAKE or fq_codel) at about 90–95% of the measured rate. The SQM tuner (§8.4) confirms the exact setting.

### 7.4 Path analysis

| Test ID | What it does | Key outputs |
|---|---|---|
| `path.trace` | Traceroute over ICMP, UDP or TCP-SYN (to the test port, which passes most firewalls). Paris mode (constant flow ID) is the default. It runs **from both ends at the same time**. | Hops, per-hop RTT, rDNS, ASN/organisation, IXP, country, **MPLS labels** (RFC 4950 ICMP extensions), private and **CGNAT** (100.64.0.0/10) hops |
| `path.multipath` | Varies the flow ID (Paris/Dublin-style) to enumerate ECMP branches | Number of distinct paths, plus a diamond graph |
| `path.mtr` | Continuous per-hop probing over N cycles | Per-hop loss, latency and jitter, **interpreted correctly**: loss at a hop that doesn't continue downstream is ICMP rate limiting, not real loss |
| `path.loaded` | MTR while the link is saturated | Shows **which hop's latency inflates first**, which locates the bloated queue ("bottleneck locator") |
| `path.asymmetry` | Compares forward and reverse paths (hops, ASNs) and OWD | Asymmetry report |
| `path.history` | Fingerprints each path and stores the changes | Route-change timeline, correlated with performance shifts |
| `path.efficiency` | Compares base RTT with the speed-of-light-in-fibre minimum for the sites' entered locations | Flags tromboning and suboptimal routing or peering |

### 7.5 MTU & middlebox integrity (tests that need control of both ends)

| Test ID | What it does | Key outputs |
|---|---|---|
| `mtu.pmtu` | Binary search with DF-set probes over ICMP and UDP. Uses PLPMTUD (RFC 8899) for a result that blackholes can't fool. Runs in both directions. | Path MTU, whether ICMP Frag-Needed / PTB messages arrive, **PMTU blackhole detection**, jumbo-frame support |
| `mtu.fragment` | Checks whether IPv4 fragments and IPv6 fragment headers pass | Fragment handling |
| `integrity.tcp` | Compares the SYN as sent with the SYN as received: MSS, window scale, SACK, timestamps, ECN, TFO | **MSS clamping**, option stripping, firewall sequence randomisation |
| `integrity.dscp` | Sends every DSCP value and compares what arrives | Matrix of preserved, remarked and bleached values, per direction |
| `integrity.ecn` | Checks whether ECT marks survive, and whether CE marks appear under load | ECN path support |
| `integrity.ttl` | Compares received TTL with the traceroute hop count | Hidden hops (MPLS tunnels, transparent devices) |
| `nat.detect` | Compares local with observed address and port | NAT present, CGNAT, NAT mapping and filtering behaviour (RFC 4787), hairpinning |
| `nat.timeout` | Holds idle UDP and TCP flows for increasing intervals | **Idle timeouts of stateful devices**, for example "TCP sessions dropped after 300 s idle". This is the classic cause of dropped SSH, database and VPN sessions, and the output includes a keepalive recommendation. |
| `integrity.tls` | Compares the certificate the agent sees with the server's real certificate | TLS interception or a transparent proxy |

### 7.6 Quality of experience

| Test ID | What it does | Key outputs |
|---|---|---|
| `qoe.voip` | Simulated RTP-like calls (G.711 at 20 ms, Opus) in both directions at once, idle and under load | **MOS and R-factor** (ITU-T G.107 E-model), late loss after a simulated jitter buffer, mouth-to-ear estimate |
| `qoe.video` | Bursty, frame-based conferencing traffic (keyframes plus delta frames) | Frame delay and frame loss |
| `qoe.gaming` | Small packets at 60–128 Hz | Latency and jitter spikes, loss |
| `qoe.web` | A mix of short flows | Page-load proxy times |

Each QoE test gives a readiness verdict: **Good**, **Acceptable** or **Poor**.

### 7.7 Host & environment diagnostics (both ends, every run)

These checks exist so a host problem is never blamed on the network.

- **CPU:**
  - per-core usage including softirq (single-core saturation is the most common cause of falsely low results)
  - steal time, and virtualisation type
- **NIC:**
  - link speed and duplex, driver and firmware, MTU
  - offloads (TSO/GSO/GRO/LRO), ring and queue counts, IRQ spread
  - **changes in error and drop counters** during the test (`ethtool -S`, softnet drops)
- **Wi-Fi**, when the endpoint is wireless: SSID, band, channel and width, RSSI and noise, MCS, tx/rx bitrate, retries.
- **Kernel/TCP:** version, rmem/wmem limits, default congestion control and qdisc, ECN, MTU probing, conntrack table usage.
- **Clock-sync state.**
- **Cross-traffic:** interface bytes minus test bytes during the test. If significant other traffic is present, the result is flagged as contended and its confidence drops.
- **Derived limits**, for example:
  - "tcp_rmem max 6 MB caps a single stream at about 1.2 Gbps at 40 ms RTT"
  - "1 GbE NIC caps TCP goodput at about 940 Mbps"

### 7.8 Standards-based modes (evidence carriers accept)

| Mode | Contents |
|---|---|
| **RFC 2544-style** | Throughput (zero-loss rate found by binary search), latency, frame loss against offered load, back-to-back bursts, per frame size (64, 128, 256, 512, 1024, 1280, 1518 bytes, and jumbo) |
| **ITU-T Y.1564-style service activation** | Stepped load at 25, 50, 75 and 100% of CIR, then EIR, then an overload/policing step, with pass/fail against FLR, FTD and FDV targets |
| **RFC 6349 TCP throughput** | Path MTU → baseline RTT → BDP → TCP throughput, reporting TCP Transfer Time Ratio, TCP Efficiency and Buffer Delay % |
| **STAMP (RFC 8762) / TWAMP-Light** (Phase 7) | Reflector and sender, for interoperability with routers and NIDs that support these protocols |

These modes are labelled "-style" honestly. RFC 2544 is defined for lab device benchmarking, and the reports say so.

## 8. Rate sweeps & traffic shaping

This section covers the "speed sweeps to see the full nature of the network" capability.

### 8.1 Rate sweep: the network response curve (`sweep.rate`)

The sweep steps the offered load from low to **beyond** capacity. Each step lasts N seconds after a settle time, and measures delivered rate, loss, RTT/OWD and jitter.

- **Modes:**
  - linear
  - logarithmic
  - **binary search** (the highest rate that stays under a loss or latency threshold)
  - a custom list of rates
- **Traffic types:**
  - UDP CBR (precise token-bucket pacing, with timer batching and GSO)
  - TCP paced via `SO_MAX_PACING_RATE`
  - TCP with N streams
- **Curve annotations:**
  - The **knee**, which is the available bandwidth: the point where delivered rate stops tracking offered rate.
  - **Onset of queueing**: latency starts to rise before loss does. The maximum queue delay gives the **buffer depth**.
  - **Signature classification:**

| Signature | Meaning |
|---|---|
| Latency stays flat, then loss jumps sharply at rate R | **Policer** at R (token bucket) |
| Latency climbs to a plateau, then loss starts | **Shaper** with a buffer at R. The plateau height is the buffer depth. |
| Latency climbs steeply and keeps climbing; loss comes late | **Bufferbloat** (deep FIFO) |
| Roughly constant loss at every rate | **Non-congestive loss** (physical layer, radio, interface errors) |
| High rate for the first T seconds, then a lower sustained rate | **Burst allowance** (a "boost" bucket) followed by a sustained rate. The output reports the bucket size and the sustained rate. |

The A→B and B→A curves are shown side by side.

### 8.2 Other sweeps

| Sweep | What it reveals |
|---|---|
| **Packet size** (64 B to MTU, plus jumbo) | The pps ceiling of firewalls and routers (small-packet performance), and MTU cliffs |
| **Burst size** (back-to-back bursts of K packets at line rate) | Burst tolerance and token-bucket depth. **Packet-train dispersion** also gives a non-intrusive estimate of bottleneck capacity, cheap enough for continuous monitoring. |
| **Stream count, window size, congestion control** | See §7.2 |
| **DSCP/QoS under load** (saturate with best effort, probe with EF, AF41 and CS1) | Whether priority queuing actually protects latency, shown as a matrix per class |
| **Soak** (steady load at X% for 1–24 h) | Periodic drops, microbursts, thermal or CPU throttling, time-of-day effects |
| **Two-dimensional matrix** (for example rate × packet size) | Heatmap of loss or latency across both dimensions |

### 8.3 Traffic-shaping engine

The engine applies shaping to an endpoint's egress, and to its ingress via IFB, for the duration of a test step.

- **Rate limiting:** tbf, htb, **CAKE** (with link-overhead and ATM compensation options), or fq_codel + htb.
- **Impairment emulation (netem):** delay; jitter (with a chosen distribution); loss (random or Gilbert-Elliott); reordering; duplication; corruption; rate.
- **Scope:**
  - **Whole interface is the default** (decision 2026-11-05). The qdisc goes on the root of the interface (egress) and on an IFB device (ingress), so all traffic on that interface is shaped while the lease lasts. Before applying it, the UI names the interface and the lease duration and asks for confirmation. The agent's policy must also allow it, and `agent join` sets the policy to `interface` by default.
  - **Test traffic only** comes later (Phase 7). Egress is straightforward: test sockets carry an `SO_MARK`, and tc filters classify only marked packets. Ingress is harder, because received packets carry no socket mark, so the IFB filters have to match each test session's 5-tuple instead. This scope should exist before the project is promoted publicly, because it is the safer default for strangers' networks.
- **Safety:**
  - Every shaping configuration is a **lease** with an expiry (step duration plus a grace period).
  - The agent removes the shaping:
    - when the lease expires
    - when it loses the control channel (dead-man switch)
    - when the service stops (`ExecStopPost`)
    - at startup, by cleaning up any qdisc carrying a PathGauge handle
  - Before shaping, the agent saves the interface's existing qdisc configuration, and it restores that configuration afterwards rather than simply deleting its own qdisc. This matters with whole-interface scope, because the box may already use fq or fq_codel.
- **Transparency:** Every `tc` command is logged and shown in the UI, and can be copied. That makes it easy to apply a recommended configuration permanently.
- **Saved profiles**, for example "LTE congested", "Satellite GEO 600 ms" or "Transatlantic 80 ms, 0.1% loss".

### 8.4 SQM tuner (shaping combined with sweeps)

The tuner answers the question "what if I enable smart queue management, and at what rate?"

1. Measure the baseline capacity and loaded latency in each direction.
2. Apply CAKE at 100%, 95%, 90%, 85% and 80% of the measured rate (or a finer search around the knee).
3. Run the bufferbloat test at each setting.
4. Plot the **throughput vs loaded-latency trade-off**, and recommend the highest rate that meets the target grade (for example A).
5. Output a ready-to-use configuration for Linux `tc`/CAKE, for OpenWrt SQM, and as generic router settings.

### 8.5 Emulation lab mode

This mode applies netem to **emulate a different network** between the two boxes (high latency, loss, low bandwidth) and then runs any profile. Teams can see how their applications behave on bad networks with the same tooling.

## 9. Test orchestration & profiles

### Run lifecycle

1. **Request.** The UI, the scheduler or the API asks for `Run(link, profile, overrides)`.
2. **Pre-flight checks:**
   - Both endpoints are online and on compatible versions.
   - Their capabilities are sufficient, and agent policy allows the run (rate, hours, test types).
   - Clock state is known.
   - Host health is acceptable; the run warns if CPU is busy or cross-traffic is high.
   - The **estimated duration and data volume** are shown and checked against the link's data budget.
3. **Locking.** The orchestrator takes exclusive **endpoint locks**, so heavy tests never overlap. Continuous probes pause, or are tagged "during test" so they don't trigger alerts.
4. **Steps.** For each step:
   - The orchestrator creates a session ID and session key.
   - It sends `PrepareTask` to the receiver/reflector side, then `StartTask` to the sender side.
   - The endpoints stream `IntervalResult` messages every 100 ms to 1 s.
   - The server stores them and pushes them to browsers over WebSocket.
5. **Guard interval.** A pause between steps (default 2 s) lets queues drain.
6. **Abort.** Possible at any time from the UI or API. Traffic stops within 1 s and shaping is cleared.
7. **Post-processing:**
   - aggregation
   - the diagnosis engine
   - scores
   - Link Fingerprint (for Full Diagnostic runs)
   - then notifications and webhooks
8. **Repeatability.** A profile can run each step N times and report the median and spread. Results are marked low-confidence when host limits or cross-traffic are detected.

### Test plugin interface (sketch)

```go
type Test interface {
    Spec() TestSpec                              // id, JSON-Schema params + defaults, required capabilities,
                                                 // estimated duration & data volume
    Plan(p Params, l LinkInfo) ([]Task, error)   // tasks per endpoint role: sender, receiver,
                                                 // reflector, prober, shaper
    Analyze(r StepResult) (Metrics, error)       // aggregate raw samples into metrics
}
```

The JSON-Schema parameters **generate the UI form automatically**. A new test appears in the dashboard without any frontend work.

### Built-in profiles

| Profile | Contents | ≈ Duration | ≈ Data at 1 Gbps* |
|---|---|---|---|
| **Quick Check** | reach, 10 s idle latency, 8-stream TCP A→B and B→A, bufferbloat-lite | 1 min | 5 GB |
| **Standard** | Quick Check plus single-stream TCP both ways, UDP loss/jitter, full bufferbloat, traceroute both ways, PMTU, host diagnostics | 4–5 min | 10–15 GB |
| **Full Diagnostic** | Standard plus stream scaling, congestion-control comparison, rate sweep both ways, packet-size sweep, DSCP/QoS, middlebox integrity, NAT, loaded MTR and discrimination tests. Produces a **Link Fingerprint**. | 15–20 min | 40–60 GB |
| **Bufferbloat & SQM** | bufferbloat plus the SQM tuner | 5–10 min | 15–30 GB |
| **Service Activation** | Y.1564-style plus RFC 6349 | 10 min | 20–40 GB |
| **VoIP/UC Readiness** | `qoe.voip` and `qoe.video`, idle and loaded | 5 min | Low |
| **Path Deep-Dive** | trace (all protocols), multipath, 5 min MTR, asymmetry | 6 min | Very low |
| **Low-Data** | latency, path, packet-train capacity estimate, short throughput burst | 1 min | < 100 MB |
| **Soak** | steady X% load for N hours, plus probes | 1–24 h | User-defined |

\* Data volume scales linearly with link speed. The UI always shows the estimate before a run starts.

## 10. Diagnosis engine

The diagnosis engine is a rules engine that turns metrics into **Findings**. Each finding has:

- severity and confidence
- a title and a plain-English explanation
- **evidence**: links to the exact chart and time range
- a recommendation
- references

Rules are typed Go code with golden-fixture tests, and their thresholds are configurable.

### Initial rule set

| Rule | Detects | Logic (simplified) | Typical recommendation |
|---|---|---|---|
| `bufferbloat.<dir>` | Excess queueing under load | p95 loaded latency − idle latency > grade threshold | Enable SQM at the rate recommended by the SQM tuner |
| `queue.fifo` | A single shared queue | Separate-flow latency inflates as much as in-flow latency | Flow-queuing AQM (fq_codel/CAKE) |
| `perflow.limit` | A per-flow cap | 1-stream rate < 50% of the best N-stream rate | Narrowed down by the three sub-rules below |
| `perflow.window` | Window limit | High rwnd/sndbuf-limited time, or BDP > buffer maximum | Exact sysctl values to raise tcp_rmem/wmem |
| `perflow.loss` | Loss limit | Observed rate ≈ Mathis bound (MSS/RTT · 1.22/√p) | Find the loss source; try BBR |
| `perflow.policer` | Per-flow policing | Each flow flat-tops at the same rate regardless of stream count | Ask the provider; use parallel transfers |
| `host.cpu` | Endpoint is CPU-bound | A core > 90% busy (including softirq) during the test | RSS/IRQ affinity, more streams. Result marked low-confidence. |
| `host.nic` | NIC or link speed cap | Throughput ≈ link speed minus protocol overhead | Faster NIC or link |
| `host.wifi` | Wi-Fi is the limit | Low RSSI or MCS; throughput follows the PHY rate | Move the AP or client, change band or channel |
| `host.errors` | Interface errors | CRC or drop counters increase during the test | Check cabling, duplex, optics |
| `rate.policer` | Policer signature in a sweep | Sharp loss onset without latency rise | Shape just below the rate; ask about CIR and burst |
| `rate.shaper` | Shaper and its buffer | Latency plateau, then loss | Informational, with buffer depth reported |
| `rate.burst` | Burst allowance | Throughput drops to a lower rate after T seconds | Plan capacity on the sustained rate |
| `loss.noncongestive` | Loss at low load | Loss when idle, or constant across rates | Physical layer, radio or line errors |
| `path.icmp_ratelimit` | **Harmless** intermediate loss | Loss at hop k does not continue to k+1…n | None. This explains away alarming mtr output. |
| `path.loss_hop` | Real loss from hop k onward | Loss continues from hop k to the destination | Escalate with evidence; owner identified by ASN |
| `path.asymmetric` | Forward and reverse paths differ | Hop or ASN mismatch | Informational; considered in OWD analysis |
| `path.changed` | Route change linked to degradation | Fingerprint change plus a step change in metrics | Escalate with the timeline |
| `path.inefficient` | Detour routing | Base RTT ≫ fibre-distance minimum | Routing or peering issue |
| `mtu.blackhole` | PMTU blackhole | Large DF packets lost with no PTB message received | Allow ICMP type 3 code 4 / ICMPv6 PTB; clamp MSS |
| `mtu.mss_clamp` | MSS rewritten in transit | SYN MSS received ≠ MSS sent | Informational (expected on tunnels) |
| `qos.bleached` / `qos.remarked` / `qos.ineffective` | DSCP stripped, rewritten, or not honoured | DSCP matrix, priority-under-load test | Fix QoS policy at the identified boundary |
| `throttle.udp` / `throttle.port` / `throttle.dpi` | Traffic discrimination | Throughput differs significantly by protocol, port or TLS wrapping | Evidence for the provider; consider tunnelling |
| `wanopt.compression` | Compression inflating results | Compressible payload ≫ random payload | Use random-payload results |
| `nat.cgnat` / `nat.timeout_short` | CGNAT; short idle timeouts | 100.64/10 hops; measured timeout | Keepalive interval below the timeout |
| `reorder.high` | Packet reordering > 1% | UDP sequence analysis | Per-packet load balancing or bonding upstream |
| `qoe.voip_poor` | Poor voice quality | MOS < 3.6 | QoS for voice, reduce jitter |
| `congestion.peak_hours` | Time-of-day degradation | Hour × weekday heatmap pattern | Contention at the ISP; escalation evidence |
| `clock.unsynced` | OWD unreliable | adjtimex/offset estimate | Configure chrony or PTP |
| `slowstart.slow` | Slow ramp-up | Time to reach 90% throughput > threshold | Initial window or congestion-control tuning |
| `ipv6.divergent` | IPv6 and IPv4 performance differ | Material gap between the dual-stack results | Check the v6 path or peering |

### Scoring

- **Per-test grades** from A+ to F, with documented thresholds.
- **Link Health Score** from 0 to 100, a weighted blend with configurable weights:

| Component | Weight |
|---|---|
| Throughput vs expected | 25 |
| Latency and jitter | 20 |
| Loss | 20 |
| Bufferbloat | 20 |
| Path stability | 10 |
| Integrity | 5 |

- **Link Fingerprint:** a one-page characterisation of the link, produced by a Full Diagnostic run. It covers:
  - bottleneck capacity, and available bandwidth per direction
  - base RTT
  - buffer depth (ms and bytes), and queue type (FIFO or flow-queued)
  - policer or shaper, with rate and burst
  - per-flow limit
  - path MTU
  - hop count and ASNs
  - NAT/CGNAT
  - DSCP behaviour
  - loss type (congestive or non-congestive)
  - the best congestion-control algorithm

*Optional, later and off by default:* an AI-written narrative summary generated from the findings only, using a provider the user configures.

## 11. Dashboard

The dashboard is a single-page app embedded in the server binary.

**Design principles:**

- Everything updates live.
- **Every number links to its evidence.**
- Units are consistent (bits/s with SI prefixes; ms to 2 decimal places).
- Light and dark themes, with a colour-blind-safe palette.
- Every view has a shareable URL.
- Annotations can be added by dragging a time range on any chart.

| Page | Purpose | Key visuals |
|---|---|---|
| **Overview** | All links at a glance | Link tiles (status, health score, grade, 24 h latency sparkline, latest A→B/B→A throughput), active alerts, running tests, latest findings |
| **Link detail** | Everything about one link | KPI strip; latency/jitter/loss time series with path-change and annotation markers; throughput history; grade history; **hour × weekday heatmap**; Link Fingerprint; recent runs |
| **New test** | Launch a run | Profile picker, direction, IP version, advanced parameters (auto-generated form), **estimated duration and data volume**, run now or schedule |
| **Live run** | Watch it happen | Step progress; per-stream stacked throughput; latency under load with phase shading; TCP_INFO (cwnd, RTT, retransmits); **CPU and NIC utilisation of both endpoints**; live hop table; Abort button |
| **Run results** | Understand the run | Scorecard, findings with evidence links, a tab per step, raw-data download, "Create report", "Compare" |
| **Bufferbloat** | Queueing behaviour | Latency CDF per phase, box plots, grade, RPM, separate-flow vs in-flow chart |
| **Sweeps** | Network response | **Response curve** (offered vs delivered rate, with loss and latency on twin axes, and the knee and signature annotated); stream-scaling bars; packet-size pps chart; DSCP matrix; sweep heatmaps; SQM-tuner trade-off chart |
| **Path** | Route analysis | Forward and reverse hop tables side by side (MTR style); **topology graph** with ECMP branches coloured by ASN; per-hop latency under load; path-history timeline; optional map |
| **Compare** | Before/after | Overlay of up to 4 runs; metric-diff table with % change and a significance indicator |
| **Monitoring** | Long-term view | Long-range charts with automatic rollup selection, percentile bands, SLA compliance, heatmaps |
| **Agents** | Fleet management | Inventory, host diagnostics, **capability matrix**, versions, policy, enrollment tokens, `doctor` output |
| **Shaping** | Shaping control | Saved profiles, **active leases with countdown and a kill switch**, tc command history |
| **Schedules / Alerts / Reports / Settings / Audit log** | Administration | — |

## 12. Reports

### Templates

| Template | Audience | Contents |
|---|---|---|
| **Executive summary** (2–4 pages) | Management | Health score, grades, top findings in plain English, recommendations, trend against the previous period |
| **Technical diagnostic** | Engineers | All metrics and charts, findings with evidence, host configurations, methodology |
| **ISP / carrier escalation pack** | Provider support | Timeline of the issue; repeated evidence across days; traceroutes in both directions with loss located to a hop; Y.1564 and RFC 6349 results; methodology and clock-sync statement. **Written to get past first-line support.** |
| **Before / after change** | Change review | Metric deltas either side of a change (for example SQM enabled, circuit upgraded, firewall replaced) |
| **SLA / periodic** (weekly or monthly) | Service management | Availability; latency, jitter and loss percentiles against SLA; throughput against contract; incidents |
| **Link Fingerprint** (1 page) | Anyone | The characterisation card from §10 |
| **Custom** | Anyone | Section builder, free-text notes and annotations |

### Features

- **Scope:** one run, several runs, a link over a time range, or several links.
- **Formats:**
  - **PDF** (Typst)
  - **self-contained HTML** (a single file that opens offline)
  - Markdown
  - CSV/JSON/Parquet data appendices
  - optional PCAP attachments
- **Branding:** logo, colours, company name, footer text.
- An **automatic methodology appendix** listing the software version, parameters, host configurations, clock sync and test definitions.
- **Scheduled generation** with email delivery.
- **Share links** that are read-only and expire.
- Report history, with regeneration from stored data.

### Pipeline

Report generation runs as a background job:

1. Choose the template and scope.
2. Run the queries.
3. Render charts with the Go SVG renderer.
4. Render HTML (Go templates) or PDF (Typst).
5. Store the file in `/var/lib/pathgauge/reports`.
6. Notify the user.

## 13. Monitoring, scheduling & alerting

- **Schedules:**
  - cron expressions
  - **blackout and maintenance windows**
  - random start jitter, so many agents don't all test at the same moment
- **Continuous lightweight probes** (per link, configurable, with the data cost shown):
  - UDP probes at 1 pps
  - a path check every 15 min
  - a packet-train capacity estimate every 30 min
  - an optional short throughput burst every hour
- **Rollups:** raw data kept for 14 days, 1-minute rollups for 90 days, 1-hour rollups for 2 years (all configurable).
- **Baselines:** a rolling baseline per hour of the week. A value counts as an anomaly when it deviates by more than k × MAD.
- **Alert rules:**
  - threshold (for example "latency p95 > X for Y minutes")
  - anomaly
  - SLA breach
  - path change
  - agent offline
  - grade drop
  - with hysteresis, de-duplication and acknowledgement
- **Notification channels:** email (SMTP), webhook, Slack, Microsoft Teams, ntfy, PagerDuty, syslog.
- **SLA tracking** against each link's expectations, with monthly compliance figures.

## 14. API, CLI & integrations

- **REST API:** OpenAPI 3, with browsable docs at `/api/docs`, and API tokens scoped by role.
- **WebSocket** stream for live run data.
- **CLI:**

```bash
pathgauge run --link hq-leeds --profile standard --wait --json
pathgauge results show <run-id> --format table
pathgauge report create --run <run-id> --template technical --format pdf -o report.pdf
pathgauge endpoint                                  # standalone endpoint (no server)
pathgauge test 203.0.113.10 --profile quick         # peer mode: prints results, saves JSON
pathgauge import results.json                       # import peer-mode results into a server
pathgauge agent status | pathgauge doctor         # check the local install & host
pathgauge tune --check                              # host tuning advice for the measured BDP
```

- **Exporters:**
  - Prometheus `/metrics` (link metrics and app health)
  - InfluxDB line protocol
  - OpenTelemetry metrics
  - a ready-made **Grafana dashboard** JSON
- **Webhooks** on run completion, new findings and alerts.
- **Data export:** CSV, JSON, Parquet (raw samples).

## 15. Security

- **Agent identity:**
  - internal CA and mTLS
  - single-use, expiring enrollment tokens
  - optional manual approval
  - automatic certificate rotation and revocation
- **Data plane:**
  - Every test step gets its own session key, and every TCP hello and UDP probe carries an HMAC. Unauthenticated packets are dropped silently.
  - The reflector **never sends more bytes than it receives** (no amplification).
  - Per-session rate caps apply.
  - Together, these stop the server being abused as a traffic cannon.
- **Web:**
  - TLS (generated self-signed, ACME, or your own certificate)
  - Argon2id password hashes and optional TOTP 2FA
  - OIDC and LDAP (Phase 7)
  - **RBAC** with three roles: Admin; Operator (can run tests and shaping); Viewer
  - CSRF protection, a strict CSP, secure cookies, login rate limiting
- **Audit log:** who ran which test, applied which shaping, and changed which setting. Tests can disrupt networks, so accountability matters.
- **Agent local policy** (`/etc/pathgauge/policy.yaml`), which the server **can't override**:
  - maximum rate and duration
  - allowed tests and allowed hours
  - shaping permission (`none`, `interface`, and from Phase 7 `test-only`). The default is `interface`, and `agent join --shaping=none` turns shaping off.
  - allowed peers
- **Least privilege:** non-root service user with specific capabilities, plus systemd sandboxing.
- **Supply chain:**
  - signed packages and repository metadata
  - SBOM and reproducible builds
  - `govulncheck` and `gosec` in CI
  - fuzzing of the wire-protocol parsers
- **Privacy:** enrichment databases are local, and there's no telemetry phoning home.

## 16. Data model & storage

| Entity | Key fields |
|---|---|
| `agents` | id, name, site, tags, version, capabilities (JSON), host facts, cert fingerprint, policy snapshot, location (lat/lon), last_seen, approved |
| `links` | id, name, endpoint_a, endpoint_b, expectations (contracted rate per direction, SLA latency/jitter/loss), data budget |
| `profiles` | id, name, built-in flag, steps (JSON: test id + params + repeat) |
| `runs` | id, link, profile, trigger (manual/schedule/api), requested_by, status, timing, overrides, environment snapshot, health score, summary |
| `steps` | id, run, test id, direction, IP version, params, status, timing, metrics (JSON), grade, confidence |
| `series` | step, series name (for example `stream3.goodput`, `probe.rtt`), encoding, zstd-compressed delta-encoded sample blob |
| `findings` | id, run, step, rule id, severity, confidence, title, explanation, evidence refs, recommendation |
| `path_snapshots` | id, link, direction, protocol, taken_at, fingerprint, hops (JSON, with enrichment) |
| `monitor_samples` + `monitor_rollup_1m` / `_1h` | link, timestamp, metric, value, or min/avg/max/p95/count |
| `schedules`, `blackouts` | cron, profile, link(s), enabled, windows |
| `alert_rules`, `alert_events`, `notification_channels` | Rule definitions, state machine, delivery log |
| `reports`, `report_templates` | Template, scope, format, file path, created_by, share token and expiry |
| `annotations` | link, time range, text, author, tags |
| `shaping_profiles`, `shaping_leases` | Profile definitions; active and past leases with exact tc commands |
| `users`, `api_tokens`, `enrollment_tokens`, `audit_log`, `settings` | — |

Sizing: a 10-minute test sampled every 100 ms with 32 streams produces about 200k samples, which compresses to well under 1 MB. SQLite in WAL mode comfortably handles about 50 agents with continuous probes. Beyond that, switch to PostgreSQL or TimescaleDB with the same schema (via sqlc).

## 17. Protocols

### Control (gRPC, agent → server, bidirectional stream)

```proto
service AgentControl {
  rpc Enroll(EnrollRequest) returns (EnrollResponse);           // token + CSR → signed cert
  rpc Connect(stream AgentMessage) returns (stream ServerMessage);
}
// ServerMessage: PrepareTask | StartTask | AbortTask | ApplyShaping | ClearShaping
//              | CollectHostInfo | UpdatePolicyView | Upgrade | Ping
// AgentMessage : Register(version, capabilities, host facts, policy) | TaskStatus
//              | IntervalResult | TaskResult | HostInfo | Log | Heartbeat | SpoolUpload
```

### Data plane: TCP session hello

```
magic "NTST" | version u8 | role u8 (send/recv) | session_id [16]
| nonce [12] | hmac_sha256(session_key, header)[:16]
→ server replies OK/ERR, then a raw payload stream (random, incompressible)
```

### Data plane: UDP probe and reflection packet (STAMP-like, four timestamps)

```
magic u32 | ver u8 | type u8 | flags u16 | session_id u64 | seq u64
| t1_tx_ns u64 | t2_rx_ns u64 | t3_tx_ns u64 | dscp_sent u8 | ttl_sent u8
| hmac[8] | padding (to requested size; reply size ≤ request size)

RTT  = (t4 − t1) − (t3 − t2)                  (no clock sync needed)
OWD  = t2 − t1 (forward), t4 − t3 (reverse)   (only when clocks are synced)
```

The receiver uses kernel RX timestamps (`SO_TIMESTAMPNS`) and reads the received TOS/TTL (`IP_RECVTOS`, `IP_RECVTTL`) to check DSCP, ECN and TTL integrity.

## 18. Repository layout

```
pathgauge/
├── cmd/pathgauge/          # main: server | agent | endpoint | test | run | report | tune | doctor
├── internal/
│   ├── engine/               # test plugins (pure, reusable)
│   │   ├── latency/  throughput/  bufferbloat/  sweep/  path/
│   │   ├── mtu/  integrity/  nat/  qoe/  standards/   # rfc2544, y1564, rfc6349
│   │   └── registry.go
│   ├── dataplane/            # endpoint listeners, session auth, packet formats, pacing
│   ├── hostinfo/             # CPU/NIC/Wi-Fi/kernel/clock collection, capability matrix
│   ├── shaping/              # tc command builder, leases, watchdog, cleanup
│   ├── control/              # gRPC services, enrollment, CA, agent sessions
│   ├── orchestrator/         # run lifecycle, locks, pre-flight, budgets
│   ├── analysis/             # metrics math, grading, rules engine, fingerprint
│   ├── enrich/               # ASN/geo/rDNS/PeeringDB lookups
│   ├── monitor/              # continuous probes, rollups, baselines
│   ├── alerting/             # rules, state machine, channels
│   ├── report/               # templates, SVG charts, HTML/Typst renderers
│   ├── api/                  # REST handlers, OpenAPI, WebSocket hub, auth/RBAC
│   └── store/                # sqlc queries, migrations (SQLite + Postgres)
├── proto/                    # protobuf definitions
├── web/                      # React + TS dashboard (Vite)
├── packaging/                # nfpm config, systemd units, postinst/prerm, SELinux module, firewall
├── deploy/                   # install.sh, Ansible role, lab Docker compose
├── lab/                      # netns test lab: topology scripts + scenario YAMLs
├── docs/                     # this plan, user guide, methodology, ADRs
└── .github/workflows/        # CI: lint, test, lab, build, package, sign, release
```

## 19. Quality strategy

### Network-namespace lab (the most important test asset)

A script builds the topology `ns-A ── veth ── ns-router (tc netem/tbf/cake, iptables) ── veth ── ns-B` and runs both endpoints inside it. **Scenarios** are YAML files describing a known impairment and the results it should produce. They run in CI on every PR.

```yaml
name: bloated-dsl-upload
impairments:
  a_to_b: { rate: 50mbit, delay: 10ms, buffer: 500ms }
  b_to_a: { rate: 10mbit, delay: 10ms, buffer: 1000ms }
profile: standard
expect:
  throughput.tcp.b_to_a.mbps: { approx: 9.5, tolerance: 5% }
  bufferbloat.grade.b_to_a:   { worse_than_or_equal: D }
  findings: [bufferbloat.b_to_a, queue.fifo]
```

Example scenarios:

- policer vs shaper
- client behind NAT (`lab/netns.sh nat on`: IPv4 masquerade plus a stateful inbound block for IPv4 and IPv6)
- per-flow policing
- MSS clamping (iptables TCPMSS)
- DSCP bleaching
- PMTU blackhole (dropping ICMP frag-needed)
- random loss vs congestive loss
- asymmetric routes
- reordering
- NAT with a short UDP timeout
- CAKE enabled (expect grade A)

### Other layers

| Layer | Approach |
|---|---|
| Unit tests | Metric maths (percentiles, jitter, MOS, Mathis bound, grading), packet encode/decode, rule logic against golden fixtures |
| Fuzzing | `go test -fuzz` on every wire parser (TCP hello, UDP probe, control messages) |
| Cross-validation | Compare against **iperf3, irtt, flent (RRUL), mtr, tracepath** on the same lab paths. Results must agree within stated tolerances. |
| Performance benchmarks | veth pairs (tens of Gbps) and real 10 GbE hardware. Throughput and CPU-per-Gbps are tracked over time to catch regressions. |
| Frontend | Vitest component tests. **Playwright end-to-end tests** against a server with a **simulated agent** that emits realistic synthetic results (the simulated agent also speeds up UI development). |
| Packaging | Install, upgrade and remove tests on Rocky 8/9/10 and Ubuntu 22.04/24.04/26.04 VMs, covering systemd, SELinux enforcing, firewalld/ufw and the capability set |
| Security | gosec, govulncheck, dependency review, a periodic third-party review before 1.0 |

## 20. Non-functional requirements

| Area | Requirement |
|---|---|
| Platforms | Rocky/RHEL/Alma 8, 9, 10; Ubuntu 22.04, 24.04, 26.04; x86_64 and aarch64 |
| Throughput | ≥ 9.4 Gbps TCP goodput on 10 GbE with ≤ 4 streams on a modern 4-core x86 host; ≥ 940 Mbps on Raspberry Pi 5-class aarch64 hardware |
| Accuracy | Throughput within ±3% of iperf3 under the same conditions; microsecond timestamp resolution using kernel RX timestamps; probe overhead < 1% of link capacity |
| Agent footprint | < 40 MB RSS and < 1% CPU when idle with continuous probes running |
| Server scale (SQLite) | 50 agents with 1 pps probes plus scheduled tests. PostgreSQL beyond that. |
| Resilience | Agents reconnect automatically with backoff; results are spooled locally (cap configurable) |
| Abort | Traffic stops within 1 s; shaping is removed within 2 s; leases guarantee removal even if the server disappears |
| UI | First load < 2 s on a LAN; live charts update at 10 Hz without jank |
| Offline | Fully functional without internet access |
| Accessibility | WCAG 2.2 AA; colour-blind-safe chart palette |

## 21. Roadmap

> **Status (November 2026).** Built and tested:
> - Phases 0–6.
> - Most of the Phase 3 and Phase 4 test catalogue.
>
> Still to do:
> - The Phase 7 items.
> - A few catalogue entries: TCP-SYN and multipath traceroute, QUIC, extra ports for port-discrimination tests, TLS interception, NAT timeout, IPv6 extension headers, and RFC 2544/Y.1564 modes.
>
> [TESTING.md](TESTING.md) describes how each part is tested.

The **MVP is Phases 0–2**. Each phase ends in something usable. Because two boxes are needed now, **Phase 1 is the first usable release**: a peer-to-peer CLI you can install on both boxes and use straight away. The dashboard follows in Phase 2.

### Phase 0: Foundations & spikes

- Repository skeleton, Go module, CI (lint, test, build), GoReleaser/nfpm producing installable rpm and deb packages, systemd units, configuration loading, logging.
- Technical spikes:
  1. UDP pacing accuracy at 1 and 10 Gbps with GSO
  2. TCP_INFO polling overhead
  3. tc lease and cleanup robustness
  4. Typst chart rendering
- netns lab scaffolding.
- Open-source repository hygiene (§24): CONTRIBUTING, SECURITY, CODE_OF_CONDUCT, issue templates, licence and secret scanning in CI. The LICENSE file is added once the licence is confirmed.
- **Exit criteria:** packages install and their services start on Rocky 9 and Ubuntu 24.04; each spike ends with a recorded decision (ADR).
- **Progress (2026-11-05):**
  - **Done:**
    - Repository skeleton, `pathgauge doctor`, configuration and policy loading, and `.deb`/`.rpm` packages for amd64 and arm64.
    - Install tests pass on all six supported distros, and the services start under systemd on Rocky 9 and Ubuntu 24.04.
    - The netns lab and its smoke test.
    - CI and release workflows; repository hygiene files.
  - **Spike 3, partly done:** `tc` run from inside the agent's systemd sandbox can add and remove qdiscs. Leases and cleanup still need building.
  - **Also done:** Apache-2.0 LICENSE and NOTICE; a dependency licence gate in CI; lab NAT mode; `doctor` advice specific to EL8 and EL9.
  - **Remaining:** spikes 1, 2 and 4, and applying the chosen product name.

### Phase 1: Test engine & peer CLI (first usable two-box release)

- Data plane with session authentication. In peer mode, the two boxes share a pre-shared key: `pathgauge endpoint` prints one, or you set it in config.
- `pathgauge endpoint` and `pathgauge test`.
- Tests:
  - `reach`, `latency.idle`, `clock`
  - `throughput.tcp` (single and multi-stream, both directions, TCP_INFO), `throughput.udp`, `throughput.streams`
  - `bufferbloat` (grades and RPM)
  - basic `path.trace` (UDP and ICMP, Paris mode, from both ends)
  - basic `sweep.rate` (stepped UDP CBR, both directions, plotted as a table; signature classification comes in Phase 4)
  - host diagnostics snapshot
- Output:
  - JSON
  - a human-readable terminal summary
  - a simple self-contained HTML report with charts, so the immediate two-box need has shareable output before the dashboard exists
- Results saved as JSON, so Phase 2's `pathgauge import` can load them into the dashboard.
- Signed `.rpm`/`.deb` from GitHub Releases.
- **Client behind NAT** (decision 2026-11-05):
  - The client side (`pathgauge test`) opens every TCP and UDP flow.
  - For server-to-client tests, the client connects and the server sends (reverse mode).
  - UDP replies go to the address and port the server observes.
  - The client sends keepalives at least every 10 s whenever a session might go idle, so the NAT binding never expires mid-test.
  - The report states when NAT is detected (§7.5 `nat.detect`), and reverse traceroutes are labelled as ending at the NAT's public address.
- **Exit criteria:**
  - Within ±3% of iperf3 in the lab at 1 Gbps and at 10 Gbps (veth).
  - Bufferbloat grades match the lab-configured buffers.
  - CI runs the lab scenarios.
  - All tests pass in the lab with `nat on`.
  - Tested on the two real boxes (Rocky 8 and Rocky 9), and then with the client behind NAT.

### Phase 2: Server, agent & core dashboard (MVP)

- Enrollment, internal CA, mTLS gRPC control, orchestrator (pre-flight checks, locks, abort), SQLite store, REST API and WebSocket.
- Dashboard: login, Overview, Agents, Links, New test (Quick and Standard profiles), **Live run**, Run results.
- Agent local policy, offline spool, `server init` / `agent join` installers, firewall helper.
- **Exit criteria:** a user installs the server on Rocky and an agent on Ubuntu from packages, enrolls the agent, runs Standard from the browser, watches live charts and revisits stored results.

### Phase 3: Path & integrity

- `path.*` (ICMP/UDP/TCP traceroute, Paris mode, multipath, MTR, loaded MTR, asymmetry, history, efficiency) and enrichment.
- `mtu.*`, `integrity.*`, `nat.*`.
- Path page with topology graph.
- **Exit criteria:** the lab detects MSS clamping, DSCP bleaching, PMTU blackholes and asymmetric routes, and correctly separates ICMP rate limiting from real loss.

### Phase 4: Sweeps & shaping

- Rate, packet-size, burst, DSCP-under-load and soak sweeps; congestion-control and window comparisons; discrimination and compressibility tests.
- RFC 2544-, Y.1564- and RFC 6349-style modes.
- **Shaping engine** with leases and watchdog; netem emulation profiles; **SQM tuner**.
- Sweeps page with response curves.
- **Exit criteria:** the lab correctly classifies policer, shaper, bufferbloat, burst-allowance and non-congestive-loss scenarios; shaping always clears itself after a forced server kill or agent network loss.

### Phase 5: Diagnosis & reports

- Full rule set, health score, Link Fingerprint.
- Compare page, annotations.
- Report templates (Executive, Technical, ISP escalation, Before/After, Fingerprint) in HTML and PDF.
- Share links.
- **Exit criteria:** every lab scenario produces its expected findings; reports render offline on both distros.

### Phase 6: Monitoring & alerting

- Schedules and blackout windows, continuous probes, rollups and baselines, heatmaps, SLA tracking.
- Alert rules and channels.
- Scheduled reports.
- Prometheus, Influx and OTel exporters; Grafana dashboard.
- **Exit criteria:** a 30-day soak on real links with no data gaps, and alerts fire within 2 minutes of an injected fault.

### Phase 7: Scale & enterprise hardening

- **Agent-to-agent mesh**, with server-coordinated UDP hole punching for agents that are both behind NAT.
- **Test-traffic-only shaping scope**: egress by `SO_MARK`, ingress by IFB 5-tuple filters (§8.3).
- PostgreSQL/TimescaleDB, OIDC/LDAP, refined RBAC, SELinux policy module.
- STAMP/TWAMP-Light interoperability.
- QoE simulations (`qoe.*`).
- Bounded **packet capture** attached to runs.
- Signed agent auto-update, optional QUIC tests, optional AI narrative summaries.

## 22. Risks & mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Go performance at 10 Gbps (GC pressure, syscall overhead) | Under-reported capacity | Buffer pools; `sendmmsg`/`recvmmsg`; UDP GSO/GRO; `SO_REUSEPORT` with multiple sockets; per-stream goroutines pinned to cores; continuous benchmarks; host-limited results flagged. Speeds above 10 Gbps are out of scope. |
| Tests disrupt production traffic | User harm, loss of trust | Duration and data estimates with confirmation; agent local policy; blackout windows; data budgets; one-click abort; audit log |
| Shaping left applied | Outage (made more serious by whole-interface scope) | Leases, dead-man switch on control loss, startup cleanup, `ExecStopPost`, restoring the saved qdisc configuration, confirmation before applying, a lab test that kills the server mid-shape. Test-traffic-only scope arrives in Phase 7. |
| Public release: strangers point it at networks they don't own, or deploy it with weak settings | Abuse complaints, reputational damage | Authenticated data plane by default (there is no "open" mode); conservative profile defaults; a responsible-use notice in the README and on first run; `SECURITY.md` with a disclosure process |
| Public release: support burden and drifting quality | Maintainer burnout | Issue templates that ask for `pathgauge doctor` output; CI lab scenarios as a regression gate; a clear statement of supported distros and kernels |
| ICMP rate limiting or deprioritisation misleads users | False diagnoses | UDP/TCP probes alongside ICMP; explicit interpretation rules; explanations in the UI |
| Clock skew between endpoints | Wrong one-way delay figures | Detection; OWD shown only when synced, with error bounds; RTT metrics as the primary measure |
| Kernel or distro feature variance (Rocky 8's 4.18 vs 6.x kernels) | Tests missing or failing | Runtime capability matrix, graceful degradation, VM test matrix in CI |
| Firewalls block the data plane | Tests fail | Single-port design, agent-initiated connections, 443 restricted mode, clear pre-flight errors |
| Server abused as a traffic source | Security incident | Per-step session HMAC, no amplification, rate caps, unauthenticated packets dropped silently |
| Scope creep | Never ships | Phased plan with exit criteria; MVP limited to Phases 0–2 |
| Geo/ASN data licensing | Legal exposure | CC-BY datasets with attribution; MaxMind optional with the user's own key |
| Users misinterpret results | Bad decisions | Confidence on every finding; methodology appendix in every report; plain-English explanations |

## 23. Open questions

Questions about speeds, scale, audience, shaping scope, licence, the first two boxes and NAT were answered on 2026-11-05 (§2). These remain open:

1. **Name.** The recommendation is **PathGauge** (binary and package `pathgauge`). Checked on 2026-11-05, it had:
   - no GitHub repositories, user or org with that name
   - no Ubuntu, Rocky or EPEL package with that name
   - no product of that name found in a web search
   - no NS records for the .dev, .io, .net or .org domains (.com is registered)

   The closest alternative, "PathMeter", is too close to the existing iPathmeter path-capacity tool. Renaming touches the binary, package, units, user, `/etc` paths and Go module, so do it before the first tagged release.
2. **SSO.** Is OIDC or LDAP needed early, or are local users enough for the MVP?
3. **Air-gapped sites.** Are any environments air-gapped? If so, offline bundles move earlier in the roadmap.
4. **Firewalls that only allow 443.** Will any client sit behind a firewall that only allows port 443? If so, the restricted mode (§6) moves earlier.

## 24. Open-source readiness

The project will probably be published on GitHub, so these items are built in from the start rather than added just before release.

### Licence

- **Chosen: Apache-2.0** (2026-11-05). It is permissive, it includes an explicit patent grant, and it is compatible with every planned dependency:
  - Go libraries (BSD, MIT, Apache)
  - React (MIT)
  - ECharts (Apache-2.0)
  - Typst (Apache-2.0)
- **Enforcement:** CI runs a licence audit (`go-licenses` for Go, `license-checker` for the web bundle) and fails on licences incompatible with the chosen one.

### Repository contents

- README with a quick start, screenshots and a **responsible-use notice**:
  - only test networks you are authorised to test
  - tests consume data and can disrupt production traffic
- LICENSE and NOTICE.
- CONTRIBUTING.md, using a DCO sign-off rather than a CLA.
- CODE_OF_CONDUCT.md (Contributor Covenant).
- SECURITY.md, using GitHub private vulnerability reporting.
- CHANGELOG, generated from Conventional Commits.
- Issue templates that ask for `pathgauge doctor` output; a PR template; Dependabot or Renovate.

### Hygiene

- **No secrets:** gitleaks runs in CI.
- **No real addresses:** examples use documentation address ranges only (192.0.2.0/24, 198.51.100.0/24 and 203.0.113.0/24 for IPv4; 2001:db8::/32 for IPv6). No internal hostnames.
- **Data-driven defaults** with no embedded keys.
- **No telemetry.**
- **No committed third-party data:** enrichment databases are downloaded, not committed.

### CI, releases and distribution

- **CI:** GitHub Actions, which is free for public repositories. The netns lab runs on the hosted Ubuntu runners (`sudo` is available there), and packaging is tested in Rocky and Ubuntu containers.
- **Releases** are triggered by a `vX.Y.Z` tag. GoReleaser then builds amd64 and arm64 and publishes to GitHub Releases:
  - `.rpm`, `.deb` and tarball packages
  - checksums
  - an SBOM (syft)
  - cosign keyless signatures and SLSA provenance
  - GPG-signed packages
- **Package repositories:** signed APT and YUM repositories hosted on GitHub Pages, or on a free open-source tier of Cloudsmith or packagecloud. `install.sh` is served from the same place and verifies signatures.
- **Docs site** on GitHub Pages, covering:
  - install and quick start
  - **test methodology** (essential for credibility)
  - how to interpret results
  - architecture
  - FAQ

### Versioning, timing and governance

- **Versioning:** SemVer, with `v0.x` until the API and protocol are stable at 1.0. The control and data-plane protocols negotiate versions from the first release.
- **Name:** before the first tag, check the candidate name for GitHub conflicts, distro package-name clashes, available domains, and trademarks. The name sets the Go module path, binary, package and systemd unit names.
- **When to go public:**
  - Develop in this repository now.
  - Optionally make it public at Phase 1 as a "preview" CLI.
  - **Announce at Phase 2**, when the dashboard exists, because first impressions decide adoption.
- **Governance:** single maintainer at first. Accept outside contributions once Phase 2's interfaces have settled.
