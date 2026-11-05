# PathGauge user guide

PathGauge measures the network path between two Linux machines and explains what it finds. There are two ways to use it:

- **Peer mode:** run `pathgauge endpoint` on one machine and `pathgauge test` on the other. There's no server or setup, and results go to the terminal, a JSON file and an HTML report.
- **Server and agents:** one machine runs the **server** (dashboard, scheduling, monitoring, alerts, reports). Other machines run an **agent** that the server controls. Agents test against the server.

Either way, **the client side opens every connection**, so the client (or agent) can be behind NAT. Only the endpoint or server must be reachable.

> Only test networks you are authorised to test. Throughput tests and sweeps fill the link, use a lot of data (about 5 GB for a quick run and 10–15 GB for a standard run at 1 Gbit/s), and slow other traffic while they run.

---

## 1. Install

The packages are for Rocky Linux / RHEL / AlmaLinux 8, 9 and 10, and Ubuntu 22.04, 24.04 and 26.04, on x86_64 and aarch64.

```bash
sudo dnf install ./pathgauge-*.x86_64.rpm      # Rocky / RHEL / Alma
sudo apt install ./pathgauge_*_amd64.deb       # Ubuntu
sudo pathgauge doctor                          # check the host is ready
```

`doctor` checks the following:

- privileges
- kernel features
- BBR
- the queueing disciplines used for shaping (CAKE, netem, IFB)
- TCP and UDP buffer limits
- clock sync
- network interfaces

Every problem comes with its impact and a fix. Two to know about:

- **Rocky 8:** `sudo dnf install kernel-modules-extra` adds netem. CAKE isn't available on EL8 kernels, so the SQM tuner uses fq_codel instead.
- **Clock sync:** enable chrony on both ends to get one-way delay per direction.

### Ports

| Port | Where | Purpose |
|---|---|---|
| 7400/tcp and 7400/udp | the endpoint or server | test traffic |
| 7443/tcp | the server | dashboard, API, agent control |

Open them on the server, for example:

- Rocky: `sudo firewall-cmd --permanent --add-port=7400/tcp --add-port=7400/udp --add-port=7443/tcp && sudo firewall-cmd --reload`
- Ubuntu: `sudo ufw allow 7400 && sudo ufw allow 7443/tcp`

Agents and peer-mode clients need no inbound ports.

---

## 2. Peer mode (two boxes, no server)

On the box that is reachable (box A):

```bash
pathgauge endpoint --key-file ~/.config/pathgauge/key   # creates a key the first time and prints the test command
```

On the other box (box B, which may be behind NAT):

```bash
pathgauge test boxA.example.com --key pg_... --profile standard --html report.html
```

### Profiles

| Profile | Tests | Time |
|---|---|---|
| `quick` | latency, TCP both ways, bufferbloat | ~1 min |
| `standard` | adds path both ways, single-stream TCP, UDP loss and jitter, PMTU, middlebox checks, voice | ~4 min |
| `full` | adds stream scaling, rate sweeps, packet-size sweep, congestion control, short flows, throttling checks, MTR and the `office` traffic mix | ~14 min |

### Useful options

- `--tests latency,bufferbloat` runs just those tests (list: `pathgauge test --help`).
- `--scale 0.5` halves every duration; `--scale 2` doubles it. Long-RTT paths automatically get at least 40 round trips per TCP test.
- `--max-rate 500` caps UDP tests and sweeps at 500 Mbit/s.
- `--expect-down 1000 --expect-up 100` adds findings when the link falls short of its contract.
- `--allow-shaping` (needs root) lets the `sqm` test shape this host's interface temporarily. See §6.
- Run `sudo pathgauge test …` to get raw-socket features: Paris traceroute with MPLS labels, and ICMP ping when ping sockets are disabled.

Results are saved as JSON under `~/.local/share/pathgauge/results/`. Render them again at any time:

```bash
pathgauge report results.json --html report.html --template executive --org "Example Ltd"
```

---

## 3. Server and agents

### Server (box A)

```bash
sudo pathgauge server init --enable        # creates the admin user, CA and certificate, then starts the service
```

Open `https://boxA:7443`. The server uses its own certificate authority, so to stop browser warnings, download `https://boxA:7443/ca.crt` and trust it. Alternatively, skip `server init` and the first visit to the dashboard asks you to create the administrator.

Create a join token, either in the dashboard (**Agents → Enrol agent**) or with:

```bash
sudo pathgauge server join-token --url https://boxA.example.com:7443
```

### Agent (box B)

```bash
sudo pathgauge agent join https://boxA.example.com:7443 --token pgjoin_... --name branch-office --enable
```

The token is single-use and expires (default 24 hours). It also carries a fingerprint of the server's CA, so the agent refuses to enrol with anything else, even if someone intercepts the connection. After joining:

- the agent connects to the server with mutual TLS
- reconnects automatically if the connection drops
- runs continuous latency monitoring (a 10-second probe burst every minute, by default)

### Agent policy

`/etc/pathgauge/policy.yaml` is enforced by the agent itself; the server can't override it. It sets:

- the maximum rate
- the maximum test duration
- which tests are allowed
- the hours when tests may run
- whether shaping is allowed (`none` or `interface`)

---

## 4. The dashboard

- **Overview:** every link (agent) with its status, health score, last results, a 24-hour latency sparkline and firing alerts.
- **Agents:** host diagnostics, settings (name, site, expected speeds, monitoring), recent runs, monitoring charts and annotations. Admins can enrol, revoke or delete agents.
- **New run:** pick an agent and a profile (or individual tests), a duration scale and a rate cap.
- **Live run:** step progress, plus throughput and latency charts while the tests run.
- **Run results:**
  - a summary and health score with its components
  - findings, each with evidence and a recommendation
  - the link fingerprint
  - every test's charts and tables
  - open, download or share the HTML report
- **Compare:** two to four runs side by side, for example before and after enabling SQM.
- **Monitoring:** latency, jitter and loss over time, with an hour × weekday heatmap that shows peak-hour congestion.
- **Schedules:** cron-style automatic runs.
- **Alerts:** rules on monitoring or run results, sent by webhook, Slack, ntfy, e-mail or written to the log.
- **Reports:** technical and executive HTML reports. **Print to PDF from the browser**; the reports include print styles. You can also import results from peer-mode runs.
- **Admin:** users and roles (viewer, operator, admin), API tokens, settings (SMTP, organisation name) and the audit log.

---

## 5. Reading the results

The **health score** (0–100) adds up six components:

| Component | Points |
|---|---|
| Throughput | 25 |
| Latency and jitter | 20 |
| Loss | 20 |
| Bufferbloat | 20 |
| Path | 10 |
| Integrity | 5 |

Each finding states what was seen, why it matters and what to do. Key ideas:

- **Bufferbloat grade:** how far latency rises under load (95th percentile). The grades are A+ (under 5 ms), A (under 30), B (under 60), C (under 200), D (under 400) and F. The cure is smart queue management (CAKE or fq_codel) at the bottleneck. The SQM tuner finds the right rate.
- **Queue type:**
  - *flow-queued*: light flows stay fast while bulk flows queue. This is good.
  - *FIFO*: everything waits in one line.
- **Single stream vs parallel:** if one connection gets much less than eight, PathGauge works out why:
  - receive window or socket buffers (it gives the exact sysctl values)
  - loss (Mathis limit)
  - a per-flow policer
  - a CPU core, above about 5 Gbit/s
  - a stream that was still ramping up when the test ended
- **Rate sweep signatures:**
  - **policer**: loss starts sharply with no queueing
  - **shaper**: delay plateaus, then loss starts
  - **bufferbloat**: hundreds of milliseconds of queueing
  - **burst allowance**: fast at first, then a lower sustained rate
  - **non-congestive loss**: loss at every rate, so suspect cabling, radio or interface errors
- **Traceroute loss:** routers rate-limit their ICMP replies, so loss at one hop that doesn't continue downstream is harmless. PathGauge only reports real path loss when its own end-to-end measurement also shows loss.
- **QoS under load (`qos` test):** EF (voice), AF41 (video) and best-effort probes run while the link is saturated. If EF's latency rises much less than best effort's, priority queuing works on the path.
- **Host limits:** UDP tests count packets the receiving host itself dropped (socket buffer full), and sweep steps the sending host couldn't generate. These are reported as *host-limited* rather than blamed on the network; this matters on small VMs and Raspberry Pis.
- **Middlebox checks:**
  - MSS clamping
  - TCP options stripped
  - DSCP preserved, rewritten or cleared
  - ECN marks preserved or cleared
  - NAT and carrier-grade NAT
  - path MTU in each direction, and whether "fragmentation needed" messages arrive (a **black hole** is when they don't)

---

## 6. Traffic shaping and the SQM tuner

The `sqm` test measures capacity and bufferbloat. It then shapes this host's interface with CAKE at 95%, 90%, 85%, 80% and 70% of the measured rate, falling back to fq_codel where there's no CAKE, measuring bufferbloat at each. It recommends the highest rate that gives grade A, with ready-to-use `tc` commands.

Safety guarantees:

- Shaping is a **lease**. It is removed when the test ends, when the lease expires (at most 1 hour), when the program stops, and at the next start if it ever crashed.
- PathGauge **refuses to touch an interface that already has a custom qdisc**, so an existing SQM setup isn't destroyed unless you force it.
- Shaping while it's active affects **all traffic on that interface**. This is the default scope; shaping only test traffic is planned.
- Agents shape only if their local policy allows it (`shaping: interface`).

---

## 7. Traffic mix and call capacity

The `mix` test answers "what would it be like to use this link?". It runs simulated application sessions at the same time and grades each one:

| Kind | What it sends | Graded on |
|---|---|---|
| `voice`, `voice-opus` | A call: 20 ms packets both ways (G.711 or Opus), marked EF | MOS (ITU-T E-model) after an adaptive jitter buffer |
| `video`, `video-hd` | A video call at 1.5 or 3.8 Mbit/s both ways: 30 frames/s, a keyframe every 2 s, paced like WebRTC, marked AF41 | Frames shown on time, freezes, and added latency |
| `gaming` | 60 small packets/s both ways | Added delay (p95) and loss |
| `web` | Page loads: an 80 KB document, then six 320 KB objects in parallel, every 5 s | Page-load time (p95) |
| `stream`, `stream-4k` | Video on demand: a 4 s chunk at 5 or 16 Mbit/s every 4 s | Chunk download time against play time (over 100% means rebuffering) |
| `download`, `upload` | A bulk TCP transfer for the whole phase | Throughput (not graded) |

Each call and game uses its own UDP port, so queues that separate flows (fq_codel, CAKE, per-flow policers) treat it like the real application. When the mix includes transfers it runs twice, **first without them and then with them**, so the results show what the transfers do to everything else, for example "voice calls drop from MOS 4.4 to 2.3 while file transfers run".

Choose the mix with `--mix` (or on the dashboard's New run page):

```bash
pathgauge test boxA --key pg_... --mix office                              # a named scenario
pathgauge test boxA --key pg_... --mix "voice:10,video:2,web:3,upload:1"   # your own
pathgauge test boxA --key pg_... --tests mix --mix family --no-dscp        # calls unmarked
```

| Scenario | Sessions |
|---|---|
| `office` (default) | 10 voice calls, 3 video calls, 5 web users, 1 download, 1 upload |
| `home-working` | 1 video call, 1 web user, 1 HD stream, 1 upload |
| `call-centre` | 30 voice calls, 5 web users, 1 download |
| `family` | 1 video call, 2 HD streams, 1 game, 2 web users, 1 download |

`--mix` on its own runs just the mix (each phase lasts 30 s × `--scale`). The `full` profile includes the mix with the `office` scenario.

**Call capacity.** `capacity-voice` and `capacity-video` find how many simultaneous calls the path carries: they double the number of calls until one turns poor, then narrow the gap, first on their own and then while a download and an upload run. A typical answer: "48 voice calls on their own, 0 while transfers run", which says the bottleneck queue needs managing. The search stops at the run's rate cap (`--max-rate`) or at 256 voice or 64 video calls.

**How calls are graded.** The jitter buffer is modelled on real call software: it follows recent delay variation, so a steady queue becomes extra delay rather than lost audio, while sudden delay jumps cause a few late packets until it adapts. Video receivers ask for lost packets again, as WebRTC does, and a frame that misses its playout time freezes the picture for as long as it's late. Video verdicts: good is at least 99% of frames on time, no freezes and at most 150 ms of added latency; acceptable is at least 95%, freezes under 2% of the time and at most 300 ms. Voice: MOS 4.0 or more is good and 3.6 or more acceptable.

Limits to keep in mind:

- The traffic copies each application's timing and packet sizes, not its protocol, so equipment that classifies traffic by inspecting packets won't recognise it as Teams or Zoom.
- Video calls use a fixed bitrate. Real applications lower it when the network is congested, so video results are the worst case for the chosen quality.
- Scheduled runs use the `office` scenario.

---

## 8. Monitoring, alerts and schedules

- **Monitoring:** each agent sends a latency, jitter and loss sample to the server every minute. You can change the interval per agent or turn it off. Monitoring pauses while a test runs. Samples are kept for 90 days (`retention_days` in `server.yaml`).
- **Alert metrics:**
  - `rtt_p95`, `loss_pct` and `jitter`: must breach the threshold for every sample in the window (`for_minutes`)
  - `offline`: minutes since the agent was last seen
  - `health_score`, `down_mbps` and `up_mbps`: evaluated when each run finishes

  A notification is sent when an alert fires and again when it resolves.
- **Schedules:** standard 5-field cron (`0 3 * * *` is 03:00 every day). If the agent is busy or offline, a scheduled run is skipped and recorded in the audit log.
- **Prometheus:** `GET /metrics` with an API token (`Authorization: Bearer pgt_...`) exposes agent status, the latest monitoring values and the latest run results.

---

## 9. Command reference

| Command | Purpose |
|---|---|
| `pathgauge doctor [--json] [--strict]` | Check host readiness |
| `pathgauge endpoint [--key K \| --key-file F] [--allow-shaping]` | Standalone test endpoint |
| `pathgauge test HOST --key K [--profile P] [--tests …] [--html F]` | Test a path in peer mode |
| `pathgauge report FILE.json --html F [--template executive] [--reanalyse]` | Render saved results (optionally with the current diagnosis rules) |
| `pathgauge tune [--rate M] [--rtt ms] [--bbr] [--apply]` | Suggest or apply TCP buffer sysctls so one stream can fill a fast, long path |
| `pathgauge enrich update` / `pathgauge enrich lookup ADDR` | Download or query the offline IP-to-operator database used to label traceroute hops (DB-IP, CC BY 4.0) |
| `pathgauge server init [--enable]` | Create the first admin and the certificates |
| `pathgauge server join-token [--hours H]` | Create an agent enrolment token |
| `pathgauge server reset-password USER` | Reset a password |
| `pathgauge server run` | Run the server (used by the systemd unit) |
| `pathgauge agent join URL --token T [--name N] [--enable]` | Enrol an agent |
| `pathgauge agent status` | Show enrolment and policy |
| `pathgauge agent run` | Run the agent (used by the systemd unit) |

---

## 10. Troubleshooting

- **`connect to endpoint … refused or timed out`:** open 7400/tcp **and** 7400/udp on the endpoint or server (host firewall and any cloud firewall).
- **Agent shows offline:** check `journalctl -u pathgauge-agent`. A message about a revoked agent means it was removed in the dashboard; enrol it again with a new token.
- **Browser certificate warning:** trust `https://SERVER:7443/ca.crt`, or click through. The certificate covers the server's hostname and IP addresses (add names under `names:` in `server.yaml`).
- **No traceroute hops:** some networks filter traceroute probes. Running as root (or through the services) enables Paris UDP traceroute, which gets through more often.
- **Hops without a network operator name:** run `pathgauge enrich update` once (as root for the services) to download the offline ASN database.
- **One stream is much slower than eight:** if the finding says socket buffers are the limit, run `sudo pathgauge tune --rate <Mbit/s> --rtt <ms> --apply` on both hosts.
- **UDP tests show loss at low rates:** check `doctor` for a small `net.core.rmem_max`. The services lift that limit automatically using CAP_NET_ADMIN.
- **Forgotten password:** `sudo pathgauge server reset-password admin`.
