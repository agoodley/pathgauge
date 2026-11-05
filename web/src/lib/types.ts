// JSON types mirroring the Go API (internal/store, internal/results,
// internal/engine, internal/hostinfo). Keep field names identical to the
// json tags in the Go code.

export type Role = 'viewer' | 'operator' | 'admin';

export interface Me {
  name: string;
  role: Role;
  user_id?: number;
  api_token?: boolean;
}

export interface User {
  id: number;
  username: string;
  role: Role;
  created_at: string;
  disabled: boolean;
}

export interface APIToken {
  id: number;
  name: string;
  role: Role;
  created_by: string;
  created_at: string;
  last_used: string;
}

export interface EnrollToken {
  id: number;
  created_by: string;
  created_at: string;
  expires_at: string;
  used_at: string;
  used_by: string;
  note: string;
}

export interface AgentPolicy {
  max_rate_mbps?: number;
  max_test_duration_ns?: number;
  allowed_tests?: string[] | null;
  allowed_hours?: string[] | null;
  shaping?: string;
  allowed_peers?: string[] | null;
}

export interface Agent {
  id: string;
  name: string;
  site: string;
  version: string;
  host?: HostReport | null;
  observed_addr: string;
  last_seen: string;
  enrolled_at: string;
  revoked: boolean;
  expected_down: number;
  expected_up: number;
  monitor_enabled: boolean;
  monitor_interval: number;
  policy?: AgentPolicy | null;
  notes: string;
  online: boolean;
  busy: boolean;
}

export type RunStatus = 'queued' | 'running' | 'done' | 'failed' | 'cancelled';

export interface RunOptions {
  profile?: string;
  tests?: string[];
  skip?: string[];
  scale?: number;
  max_rate_mbps?: number;
  streams?: number;
  client_name?: string;
  server_name?: string;
  imported?: boolean;
  client?: string;
  endpoint?: string;
}

export interface Run {
  id: string;
  agent_id: string;
  agent_name?: string;
  profile: string;
  options: RunOptions | null;
  status: RunStatus;
  trigger: string;
  requested_by: string;
  created_at: string;
  started_at: string;
  ended_at: string;
  error: string;
  health_score: number | null;
  down_mbps: number | null;
  up_mbps: number | null;
  rtt_ms: number | null;
  bloat_grade: string;
  findings?: Finding[] | null;
  report?: Report | null;
}

export interface Sample {
  ts: number;
  rtt_min: number;
  rtt_p50: number;
  rtt_p95: number;
  rtt_max: number;
  jitter: number;
  loss_pct: number;
  sent: number;
}

export interface HeatCell {
  weekday: number;
  hour: number;
  rtt_p95: number;
  loss_pct: number;
  n: number;
}

export interface Schedule {
  id: number;
  agent_id: string;
  profile: string;
  tests: string;
  cron: string;
  enabled: boolean;
  created_at?: string;
  last_run?: string;
  note: string;
}

export type AlertMetric = 'rtt_p95' | 'loss_pct' | 'jitter' | 'offline' | 'health_score' | 'down_mbps' | 'up_mbps';
export type AlertChannel = 'webhook' | 'slack' | 'ntfy' | 'email' | 'log';

export interface AlertRule {
  id: number;
  name: string;
  agent_id: string;
  metric: AlertMetric;
  op: '>' | '<';
  threshold: number;
  for_minutes: number;
  channel: AlertChannel;
  target: string;
  enabled: boolean;
  created_at?: string;
}

export interface AlertEvent {
  id: number;
  rule_id: number;
  rule_name: string;
  agent_id: string;
  state: 'firing' | 'resolved';
  value: number;
  message: string;
  started_at: string;
  resolved_at: string;
  notify_error: string;
}

export interface AuditEntry {
  id: number;
  ts: string;
  actor: string;
  action: string;
  target: string;
  detail: string;
}

export interface Annotation {
  id: number;
  agent_id: string;
  ts_start: number;
  ts_end: number;
  text: string;
  author: string;
}

export interface OverviewLink {
  agent: Agent;
  last_run?: Run;
  latest_sample?: Sample;
  spark_rtt_p95: number[];
  alerts_firing: number;
}

export interface Overview {
  links: OverviewLink[];
  running: Run[];
  recent: Run[];
  alerts: AlertEvent[];
  server_name: string;
  ca_fingerprint: string;
}

export interface Profiles {
  profiles: Record<string, string[]>;
  tests: string[];
  titles: Record<string, string>;
}

export interface CompareItem {
  id: string;
  agent: string;
  started_at: string;
  profile: string;
  summary: Summary;
  findings: Finding[] | null;
}

export type Settings = Record<string, string>;

// ---- host diagnostics (internal/hostinfo) ----

export type CheckStatus = 'ok' | 'warn' | 'missing' | 'info';

export interface HostCheck {
  section: string;
  name: string;
  status: CheckStatus;
  detail: string;
  impact?: string;
  fix?: string;
}

export interface HostInterface {
  name: string;
  oper_state: string;
  mtu: number;
  speed_mbps: number;
  driver?: string;
  wireless: boolean;
  virtual: boolean;
}

export interface HostReport {
  hostname: string;
  os: string;
  os_id: string;
  kernel: string;
  arch: string;
  cpus: number;
  virtualization?: string;
  interfaces: HostInterface[] | null;
  checks: HostCheck[] | null;
}

// ---- results (internal/results) ----

export interface StatSummary {
  n: number;
  min: number;
  mean: number;
  median: number;
  p90: number;
  p95: number;
  p99: number;
  max: number;
  stddev: number;
}

export interface LossStats {
  sent: number;
  received: number;
  lost: number;
  loss_pct: number;
  duplicates: number;
  reordered: number;
  reorder_pct: number;
  bursts: number;
  max_burst: number;
}

export interface Peer {
  name: string;
  address: string;
  local_addr?: string;
  observed_addr?: string;
  software?: string;
  host?: HostReport | null;
}

export interface Step {
  id: string;
  test: string;
  title: string;
  status: 'ok' | 'error' | 'skipped' | string;
  error?: string;
  started_at: string;
  seconds: number;
  result?: unknown;
}

export interface Finding {
  id: string;
  severity: 'critical' | 'warning' | 'info' | 'good' | string;
  confidence: string;
  title: string;
  detail: string;
  recommendation?: string;
  evidence?: string[];
}

export interface Summary {
  health_score: number;
  score_parts: Record<string, number> | null;
  down_mbps: number;
  up_mbps: number;
  down_single_mbps: number;
  up_single_mbps: number;
  idle_rtt_ms: number;
  jitter_ms: number;
  loss_pct: number;
  bloat_grade: string;
  rpm: number;
  mos: number;
  path_mtu: number;
  hops_up: number;
  hops_down: number;
  nat: boolean;
  fingerprint?: Record<string, string> | null;
}

export interface Report {
  schema: number;
  id: string;
  software: string;
  profile: string;
  started_at: string;
  ended_at: string;
  client: Peer;
  endpoint: Peer;
  steps: Step[] | null;
  findings: Finding[] | null;
  summary: Summary;
  notes?: string[];
  labels: { client: string; endpoint: string };
}

export interface Clock {
  synced: boolean;
  est_error_ms: number;
  max_error_ms: number;
}

export interface InfoResult {
  client_host?: HostReport | null;
  endpoint_host?: HostReport | null;
  client_clock: Clock;
  endpoint_clock: Clock;
  endpoint_policy: { max_rate_mbps: number; max_test_duration_s: number; shaping: string };
  clock_offset_ms: number;
  owd_trustworthy: boolean;
  observed_addr: string;
  client_local_addr: string;
}

export interface LatencyResult {
  duration_s: number;
  rate_hz: number;
  udp_rtt_ms: StatSummary;
  udp_jitter_ms: number;
  ipdv_ms: number;
  loss: LossStats;
  up_jitter_ms: number;
  down_jitter_ms: number;
  owd_up_ms?: StatSummary | null;
  owd_down_ms?: StatSummary | null;
  icmp_rtt_ms?: StatSummary | null;
  icmp_loss_pct: number;
  icmp_note?: string;
  tcp_connect_ms?: StatSummary | null;
  series_ms: number[] | null;
  clock_offset_ms: number;
}

export interface TCPInterval {
  t: number;
  mbps: number;
  per_stream?: number[];
  rtt_ms: number;
  cwnd: number;
  retrans: number;
}

export interface TCPResult {
  direction: string;
  streams: number;
  duration_s: number;
  omit_s: number;
  congestion: string;
  mbps: number;
  peak_mbps: number;
  per_stream_mbps: number[] | null;
  fairness: number;
  bytes: number;
  retransmits: number;
  retrans_pct: number;
  rtt_ms: StatSummary;
  min_rtt_ms: number;
  rwnd_limited_pct: number;
  sndbuf_limited_pct: number;
  mss: number;
  ramp_s: number;
  intervals: TCPInterval[] | null;
  compressible?: boolean;
  pacing_mbps?: number;
}

export interface UDPBin {
  t: number;
  mbps: number;
  received: number;
  queue_delay_ms: number;
}

export interface UDPResult {
  direction: string;
  target_mbps: number;
  size_bytes: number;
  duration_s: number;
  sent_mbps: number;
  mbps: number;
  pps: number;
  loss: LossStats;
  jitter_ms: number;
  queue_delay_ms: StatSummary;
  owd_ms?: StatSummary | null;
  tos_sent?: number;
  /** Packets dropped by the receiving host itself (socket buffer), not the network. */
  host_drops?: number;
  /** Loss with host drops removed. */
  net_loss_pct?: number;
  bins?: UDPBin[] | null;
}

export interface StreamsResult {
  direction: string;
  points: { streams: number; mbps: number; per_stream_mbps: number[] | null; retransmits: number; rtt_ms: number }[] | null;
  best_mbps: number;
  single_stream_efficiency: number;
}

export interface BloatPhase {
  name: string;
  rtt_ms: StatSummary;
  in_flow_rtt_ms: StatSummary;
  connect_ms: StatSummary;
  down_mbps: number;
  up_mbps: number;
  probe_loss_pct: number;
  series: { t: number; rtt_ms: number; phase: string }[] | null;
}

export interface BloatResult {
  phases: BloatPhase[] | null;
  increase_ms: Record<string, number> | null;
  grades: Record<string, string> | null;
  grade: string;
  rpm: number;
  working_latency_ms: number;
  queueing: string;
  buffer_ms?: Record<string, number> | null;
  buffer_bytes?: Record<string, number> | null;
  streams: number;
}

export interface Hop {
  ttl: number;
  addr?: string;
  name?: string;
  asn?: string;
  org?: string;
  private?: boolean;
  cgnat?: boolean;
  sent: number;
  received: number;
  loss_pct: number;
  rtt_ms: StatSummary;
  addrs?: string[];
  mpls?: string[];
  dest?: boolean;
}

export interface TraceResult {
  direction: string;
  from: string;
  target: string;
  method: string;
  cycles: number;
  hops: Hop[] | null;
  reached: boolean;
  note?: string;
  loss_from_hop?: number;
  rate_limited_hops?: number[];
  fingerprint: string;
}

export interface PMTUDir {
  direction: string;
  mtu: number;
  ptb_seen: boolean;
  ptb_mtu?: number;
  blackhole: boolean;
  local_mtu: number;
  tested: { size: number; ok: boolean }[] | null;
  note?: string;
}

export interface PMTUResult {
  up: PMTUDir;
  down: PMTUDir;
}

export interface DSCPResult {
  name: string;
  sent: number;
  received: number;
  status: string;
}

export interface IntegrityResult {
  dscp_up: DSCPResult[] | null;
  dscp_down: DSCPResult[] | null;
  ecn_up: string;
  ecn_down: string;
  client_adv_mss: number;
  endpoint_snd_mss: number;
  endpoint_adv_mss: number;
  client_snd_mss: number;
  mss_clamped_up: boolean;
  mss_clamped_down: boolean;
  client_tcp_options: string[] | null;
  endpoint_tcp_options: string[] | null;
  stripped_tcp_options?: string[] | null;
  nat: boolean;
  client_local: string;
  client_observed: string;
  port_preserved: boolean;
  cgnat: boolean;
  ttl_received_up: number;
  ttl_received_down: number;
  hops_up: number;
  hops_down: number;
}

export interface VoIPDir {
  loss: LossStats;
  jitter_ms: number;
  late_loss_pct: number;
  mos: number;
  r_factor: number;
  verdict: string;
}

export interface VoIPResult {
  codec: string;
  duration_s: number;
  one_way_ms: number;
  loaded: boolean;
  up: VoIPDir;
  down: VoIPDir;
  verdict: string;
}

export interface SweepStep {
  offered_mbps: number;
  sent_mbps: number;
  delivered_mbps: number;
  loss_pct: number;
  queue_p50_ms: number;
  queue_p95_ms: number;
  jitter_ms: number;
  net_loss_pct?: number;
  host_drop_pct?: number;
  host_limited?: boolean;
  first_half_mbps: number;
  second_half_mbps: number;
}

export interface SweepResult {
  direction: string;
  size_bytes: number;
  step_s: number;
  steps: SweepStep[] | null;
  capacity_mbps: number;
  knee_mbps: number;
  queue_onset_mbps: number;
  max_queue_ms: number;
  buffer_bytes: number;
  signature: string;
  signature_detail: string;
}

export interface SizeSweepResult {
  direction: string;
  rate_mbps: number;
  points: { size: number; offered_mbps: number; delivered_mbps: number; pps: number; loss_pct: number }[] | null;
  max_pps: number;
}

export interface CCResult {
  direction: string;
  points: { algorithm: string; mbps: number; retransmits: number; rtt_ms: number; error?: string }[] | null;
  best: string;
}

export interface ShortFlowResult {
  connect_ms: StatSummary;
  completion_ms: Record<string, StatSummary> | null;
  count: number;
  errors: number;
}

export interface DiscriminationResult {
  direction: string;
  points: { label: string; mbps: number; error?: string }[] | null;
  verdict: string;
}

export interface SQMStep {
  percent: number;
  down_shaped_mbps: number;
  up_shaped_mbps: number;
  qdisc: string;
  grade: string;
  increase_ms: number;
  down_measured_mbps: number;
  up_measured_mbps: number;
  error?: string;
}

export interface SQMResult {
  interface: string;
  baseline_down_mbps: number;
  baseline_up_mbps: number;
  baseline_grade: string;
  steps: SQMStep[] | null;
  recommended?: SQMStep | null;
  tc_commands?: string[] | null;
  note?: string;
}

export interface QoSClass {
  name: string;
  dscp: number;
  idle_rtt_ms: StatSummary;
  loaded_rtt_ms: StatSummary;
  /** Loaded p95 minus idle median. */
  increase_ms: number;
  loss_pct: number;
}

export interface QoSResult {
  classes: QoSClass[] | null;
  down_mbps: number;
  up_mbps: number;
  priority_effective: boolean;
  verdict: string;
}

// ---- live (internal/engine Event, internal/server hub) ----

export interface EngineEvent {
  type: 'run_start' | 'step_start' | 'progress' | 'step_done' | 'run_done' | string;
  run_id?: string;
  step?: string;
  test?: string;
  title?: string;
  index?: number;
  total?: number;
  status?: string;
  error?: string;
  seconds?: number;
  data?: unknown;
}

export type LiveMessage =
  | { type: 'run_event'; run_id: string; agent_id: string; seq?: number; event: EngineEvent }
  | { type: 'run_status'; run_id: string; agent_id?: string; status: RunStatus; error?: string; health_score?: number }
  | { type: 'agent_status'; agent_id: string; online: boolean }
  | { type: 'monitor'; agent_id: string; samples: Sample[] }
  | { type: 'alert'; state: 'firing' | 'resolved'; event: AlertEvent };
