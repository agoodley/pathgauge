# PathGauge REST API

Everything the dashboard does goes through this API. Base URL: `https://SERVER:7443`. All bodies are JSON.

## Authentication

- **Browser sessions:**
  - `POST /api/auth/login {"username","password"}` sets an `HttpOnly`, `Secure`, `SameSite=Strict` cookie.
  - State-changing requests (anything except GET) must also send `X-PathGauge: 1`.
- **Automation:** send `Authorization: Bearer pgt_…` with an API token from **Admin → API tokens**. The `X-PathGauge` header isn't needed.
- **Roles:** `viewer` < `operator` < `admin`. Each endpoint below lists the minimum role.

Errors look like `{"error": "message"}`, with a 4xx/5xx status.

## Endpoints

| Method and path | Role | Purpose |
|---|---|---|
| `GET /api/health` | — | Liveness and version |
| `GET /api/setup` / `POST /api/setup` | — | First-run state / create the first admin (only while no users exist) |
| `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` | —/—/viewer | Sessions |
| `GET /ca.crt` | — | The server's CA certificate |
| `POST /api/agent/enroll` | join token | Agent enrolment (used by `pathgauge agent join`) |
| `GET /api/agent/connect` | agent mTLS | Agent control WebSocket |
| `GET /api/overview` | viewer | Links with last run, latest sample, 24-hour sparkline, alerts; running and recent runs |
| `GET /api/agents`, `GET /api/agents/{id}` | viewer | Agents (detail includes the host diagnostics report) |
| `PUT /api/agents/{id}` | operator | `name, site, expected_down, expected_up, monitor_enabled, monitor_interval, notes` |
| `POST /api/agents/{id}/revoke`, `DELETE /api/agents/{id}` | admin | Revoke (disconnects; data-plane key disabled) / delete |
| `GET/POST /api/enroll-tokens`, `DELETE /api/enroll-tokens/{id}` | admin | Join tokens: `POST {hours, note}` → `{token, join_command}` |
| `GET /api/profiles` | viewer | Profiles, test IDs and their titles; traffic-mix session kinds (`mix_kinds`: name, label, class, max), scenarios (`mix_scenarios`) and the default scenario (`mix_default`) |
| `POST /api/runs` | operator | Start a run: `{agent_id, profile, tests?, skip?, scale?, max_rate_mbps?, streams?, trace_cycles?, mix?, no_dscp?}` → `{run_id}`. `mix` is a scenario name or `kind:count,...` for the `mix` test; `no_dscp` sends simulated calls unmarked; `cdp` configures the `cdp-up`/`cdp-down` tests: `{rate_mbps, burst_factor, burst_s, period_s, limit_mbps, streams, rpo_s, duration_s}` (zero means the default). An entry in `tests` that joins tests with `+` (`"voip+cdp-up+tcp-down"`) is a group run together; `group_duration_s` sets how long each grouped test runs and `no_baseline` skips running each on its own first. Reports then carry `groups` and `load_comparisons` (each grouped test's key results alone and together). Returns 400 for an invalid mix and 409 if the agent is busy. |
| `GET /api/runs?agent=&status=&limit=` | viewer | Run list (newest first) |
| `GET /api/runs/{id}` | viewer | Run with the full `report` ([results schema](../internal/results/results.go)). `agent_away: true` on a running run means its agent has lost its connection; the test continues on the agent. |
| `POST /api/runs/{id}/cancel`, `DELETE /api/runs/{id}` | operator | Cancel / delete |
| `GET /api/runs/{id}/report.html?template=technical\|executive&download=1` | viewer | HTML report |
| `POST /api/runs/{id}/share {hours, template}` | operator | Read-only share link (`/share/{token}`, no login) |
| `GET /api/compare?runs=A,B[,C,D]` | viewer | Side-by-side summaries and findings |
| `POST /api/import?agent=` | operator | Import a results JSON file from `pathgauge test` |
| `GET /api/monitor/{agent}?hours=24[&step=s]` | viewer | Monitoring samples, aggregated per step. `rtt_min`, `rtt_p50`, `rtt_p95`, `rtt_max` and `jitter` are `null` for a step in which no probe was answered (an outage); `loss_pct` is 100 there. |
| `GET /api/monitor/{agent}/heatmap?days=28` | viewer | Hour × weekday averages (UTC). `rtt_p95` is `null` for an hour with no answered probe. |
| `GET /api/annotations/{agent}`, `POST /api/annotations`, `DELETE /api/annotations/{id}` | viewer/operator | Time-range notes on charts |
| `GET/POST /api/schedules`, `PUT/DELETE /api/schedules/{id}` | viewer/operator | `{agent_id, profile, tests, cron, enabled, note}` |
| `GET/POST /api/alert-rules`, `PUT/DELETE /api/alert-rules/{id}`, `POST /api/alert-rules/{id}/test` | viewer/operator | `{name, agent_id, metric, op, threshold, for_minutes, channel, target, enabled}` |
| `GET /api/alerts` | viewer | Alert events (firing and resolved) |
| `GET /api/live` | viewer | WebSocket: `run_event`, `run_status`, `agent_status`, `monitor`, `alert` |
| `GET /metrics` | viewer | Prometheus text format. The monitoring latency and jitter gauges are left out for an agent whose latest sample had no answered probe. |
| `GET/POST /api/users`, `PUT/DELETE /api/users/{id}` | admin | Users |
| `GET/POST /api/tokens`, `DELETE /api/tokens/{id}` | admin | API tokens (`POST {name, role}` returns the token once) |
| `GET/PUT /api/settings` | admin | `server_name, org_name, smtp_host, smtp_port, smtp_user, smtp_password, smtp_from` |
| `GET /api/audit` | admin | Audit log |

### Alert rule fields

- **Metrics:**
  - `rtt_p95`, `loss_pct`, `jitter`: monitoring, sustained for `for_minutes`
  - `offline`: minutes since the agent was last seen
  - `health_score`, `down_mbps`, `up_mbps`: evaluated after each run
- **Operators:** `>` or `<`
- **Channels:**
  - `webhook`: POSTs JSON including a `text` field
  - `slack`: Slack-compatible `{"text"}`
  - `ntfy`: plain text
  - `email`: uses the SMTP settings
  - `log`

## Example

```bash
TOKEN=pgt_...
curl -sk -H "Authorization: Bearer $TOKEN" https://server:7443/api/agents
curl -sk -H "Authorization: Bearer $TOKEN" -d '{"agent_id":"agt_…","profile":"quick"}' https://server:7443/api/runs
curl -sk -H "Authorization: Bearer $TOKEN" "https://server:7443/api/runs/RUN_ID/report.html?template=executive" -o report.html
```
