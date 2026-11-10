// Package results defines the JSON result schema shared by the CLI, the
// HTML report, the server, the dashboard and the analysis rules.
//
// Units: latency in milliseconds, rates in Mbit/s, sizes in bytes,
// durations in seconds, percentages 0-100. Direction "up" is client to
// endpoint (agent to server); "down" is endpoint to client.
package results

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/agoodley/pathgauge/internal/hostinfo"
	"github.com/agoodley/pathgauge/internal/stats"
)

// SchemaVersion is bumped on incompatible result changes.
const SchemaVersion = 1

// Direction of a measurement.
type Direction string

// Directions.
const (
	Up    Direction = "up"   // client → endpoint
	Down  Direction = "down" // endpoint → client
	Bidir Direction = "bidir"
)

// Report is one complete run.
type Report struct {
	Schema    int        `json:"schema"`
	ID        string     `json:"id"`
	Software  string     `json:"software"`
	Profile   string     `json:"profile"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   time.Time  `json:"ended_at"`
	Client    Peer       `json:"client"`
	Endpoint  Peer       `json:"endpoint"`
	Steps     []Step     `json:"steps"`
	Findings  []Finding  `json:"findings"`
	Summary   Summary    `json:"summary"`
	Notes     []string   `json:"notes,omitempty"`
	Labels    LinkLabels `json:"labels"`
	// Groups are sets of tests run at the same time; LoadComparisons
	// compare each test's key results alone and together.
	Groups          []Group          `json:"groups,omitempty"`
	LoadComparisons []LoadComparison `json:"load_comparisons,omitempty"`
}

// Group is a set of tests run at the same time.
type Group struct {
	ID        string   `json:"id"`
	Tests     []string `json:"tests"`
	DurationS float64  `json:"duration_s"`
	Baseline  bool     `json:"baseline"` // each test also ran on its own first
}

// LoadComparison is one key result of a grouped test, on its own and
// while the group's other tests ran.
type LoadComparison struct {
	Group    string   `json:"group"`
	Test     string   `json:"test"`
	Title    string   `json:"title"`
	Others   []string `json:"others"` // tests that ran alongside
	Metric   string   `json:"metric"`
	Unit     string   `json:"unit"`
	Alone    float64  `json:"alone"`
	Together float64  `json:"together"`
	Better   string   `json:"better"` // "higher" or "lower"
	Worse    bool     `json:"worse"`  // noticeably worse together
}

// LinkLabels give friendly names to the two ends and directions.
type LinkLabels struct {
	Client   string `json:"client"`
	Endpoint string `json:"endpoint"`
}

// Peer describes one end of the path.
type Peer struct {
	Name         string           `json:"name"`
	Address      string           `json:"address"`
	LocalAddr    string           `json:"local_addr,omitempty"`
	ObservedAddr string           `json:"observed_addr,omitempty"`
	Software     string           `json:"software,omitempty"`
	Host         *hostinfo.Report `json:"host,omitempty"`
}

// NoteCancelled marks a report whose run was stopped before it finished.
const NoteCancelled = "run cancelled before completion"

// Cancelled reports whether the run was stopped before it finished, so the
// report holds only the steps that had run by then.
func (r *Report) Cancelled() bool { return slices.Contains(r.Notes, NoteCancelled) }

// Step is one test within a run.
type Step struct {
	ID        string    `json:"id"`
	Test      string    `json:"test"`
	Title     string    `json:"title"`
	Status    string    `json:"status"` // ok, error, skipped
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Seconds   float64   `json:"seconds"`
	Result    any       `json:"result,omitempty"`
	// Group and Phase mark tests run in a group: "alone" for the baseline
	// run on its own, "together" for the run alongside the group's others.
	Group string `json:"group,omitempty"`
	Phase string `json:"phase,omitempty"`
}

// stepAlias avoids recursion in UnmarshalJSON.
type stepAlias Step

// UnmarshalJSON decodes Result into the concrete type for Test.
func (s *Step) UnmarshalJSON(b []byte) error {
	var raw struct {
		stepAlias
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = Step(raw.stepAlias)
	if len(raw.Result) == 0 || string(raw.Result) == "null" {
		return nil
	}
	v := NewResult(s.Test)
	if v == nil {
		var generic any
		if err := json.Unmarshal(raw.Result, &generic); err != nil {
			return err
		}
		s.Result = generic
		return nil
	}
	if err := json.Unmarshal(raw.Result, v); err != nil {
		return fmt.Errorf("step %s (%s): %w", s.ID, s.Test, err)
	}
	s.Result = v
	return nil
}

// NewResult returns a pointer to the result type for a test ID.
func NewResult(test string) any {
	switch test {
	case "info":
		return &Info{}
	case "latency":
		return &Latency{}
	case "tcp":
		return &TCP{}
	case "udp":
		return &UDP{}
	case "streams":
		return &StreamScaling{}
	case "bufferbloat":
		return &Bufferbloat{}
	case "trace", "mtr":
		return &Trace{}
	case "pmtu":
		return &PMTU{}
	case "integrity":
		return &Integrity{}
	case "voip":
		return &VoIP{}
	case "mix":
		return &Mix{}
	case "capacity":
		return &Capacity{}
	case "cdp":
		return &CDP{}
	case "sweep":
		return &Sweep{}
	case "sizesweep":
		return &SizeSweep{}
	case "cc":
		return &CCCompare{}
	case "shortflow":
		return &ShortFlow{}
	case "discrimination":
		return &Discrimination{}
	case "sqm":
		return &SQMTuner{}
	case "qos":
		return &QoS{}
	}
	return nil
}

// Info records both ends' environment.
type Info struct {
	ClientHost      *hostinfo.Report `json:"client_host,omitempty"`
	EndpointHost    *hostinfo.Report `json:"endpoint_host,omitempty"`
	ClientClock     Clock            `json:"client_clock"`
	EndpointClock   Clock            `json:"endpoint_clock"`
	EndpointPolicy  Policy           `json:"endpoint_policy"`
	ClockOffsetMs   float64          `json:"clock_offset_ms"`
	OWDTrustworthy  bool             `json:"owd_trustworthy"`
	ObservedAddr    string           `json:"observed_addr"`
	ClientLocalAddr string           `json:"client_local_addr"`
}

// Clock is a host's synchronisation state.
type Clock struct {
	Synced        bool    `json:"synced"`
	EstErrorMs    float64 `json:"est_error_ms"`
	MaxErrorMs    float64 `json:"max_error_ms"`
	UnixNanoAtMsg int64   `json:"unix_nano,omitempty"`
}

// Policy is the endpoint's local safety policy as it reports it.
type Policy struct {
	MaxRateMbps        int    `json:"max_rate_mbps"`
	MaxTestDurationSec int    `json:"max_test_duration_s"`
	Shaping            string `json:"shaping"`
}

// Latency is idle round-trip measurement.
type Latency struct {
	DurationS     float64         `json:"duration_s"`
	RateHz        float64         `json:"rate_hz"`
	UDP           stats.Summary   `json:"udp_rtt_ms"`
	UDPJitterMs   float64         `json:"udp_jitter_ms"`
	IPDVms        float64         `json:"ipdv_ms"`
	Loss          stats.LossStats `json:"loss"`
	UpJitterMs    float64         `json:"up_jitter_ms"`
	DownJitterMs  float64         `json:"down_jitter_ms"`
	OWDUp         *stats.Summary  `json:"owd_up_ms,omitempty"`
	OWDDown       *stats.Summary  `json:"owd_down_ms,omitempty"`
	ICMP          *stats.Summary  `json:"icmp_rtt_ms,omitempty"`
	ICMPLossPct   float64         `json:"icmp_loss_pct"`
	ICMPNote      string          `json:"icmp_note,omitempty"`
	TCPConnect    *stats.Summary  `json:"tcp_connect_ms,omitempty"`
	Series        []float64       `json:"series_ms"`
	ClockOffsetMs float64         `json:"clock_offset_ms"`
	RecvTOS       map[string]int  `json:"-"`
	Extra         map[string]any  `json:"extra,omitempty"`
}

// TCPInterval is one sampling interval of a TCP test.
type TCPInterval struct {
	T         float64   `json:"t"`
	Mbps      float64   `json:"mbps"`
	PerStream []float64 `json:"per_stream,omitempty"`
	RTTms     float64   `json:"rtt_ms"`
	Cwnd      float64   `json:"cwnd"`
	Retrans   uint32    `json:"retrans"`
}

// TCP is a bulk TCP throughput test in one direction.
type TCP struct {
	Direction        Direction     `json:"direction"`
	Streams          int           `json:"streams"`
	DurationS        float64       `json:"duration_s"`
	OmitS            float64       `json:"omit_s"`
	Congestion       string        `json:"congestion"`
	Mbps             float64       `json:"mbps"`
	PeakMbps         float64       `json:"peak_mbps"`
	PerStreamMbps    []float64     `json:"per_stream_mbps"`
	Fairness         float64       `json:"fairness"`
	Bytes            uint64        `json:"bytes"`
	Retransmits      uint64        `json:"retransmits"`
	RetransPct       float64       `json:"retrans_pct"`
	RTT              stats.Summary `json:"rtt_ms"`
	MinRTTms         float64       `json:"min_rtt_ms"`
	RwndLimitedPct   float64       `json:"rwnd_limited_pct"`
	SndbufLimitedPct float64       `json:"sndbuf_limited_pct"`
	MSS              uint32        `json:"mss"`
	RampS            float64       `json:"ramp_s"`
	Intervals        []TCPInterval `json:"intervals"`
	Compressible     bool          `json:"compressible,omitempty"`
	PacingMbps       float64       `json:"pacing_mbps,omitempty"`
}

// UDP is a paced UDP stream in one direction.
type UDP struct {
	Direction  Direction       `json:"direction"`
	TargetMbps float64         `json:"target_mbps"`
	SizeBytes  int             `json:"size_bytes"`
	DurationS  float64         `json:"duration_s"`
	SentMbps   float64         `json:"sent_mbps"`
	Mbps       float64         `json:"mbps"`
	PPS        float64         `json:"pps"`
	Loss       stats.LossStats `json:"loss"`
	JitterMs   float64         `json:"jitter_ms"`
	QueueDelay stats.Summary   `json:"queue_delay_ms"`
	OWD        *stats.Summary  `json:"owd_ms,omitempty"`
	TOSSent    int             `json:"tos_sent"`
	// HostDrops are packets dropped by the receiving host's socket buffer
	// (not the network); NetLossPct is loss with them removed.
	HostDrops   int64          `json:"host_drops"`
	NetLossPct  float64        `json:"net_loss_pct"`
	TOSReceived map[int]uint64 `json:"tos_received,omitempty"`
	TTLReceived map[int]uint64 `json:"ttl_received,omitempty"`
	Bins        []UDPBin       `json:"bins,omitempty"`
	MinOWDRawMs float64        `json:"-"`
}

// UDPBin is one time bin of a UDP stream at the receiver.
type UDPBin struct {
	T            float64 `json:"t"`
	Mbps         float64 `json:"mbps"`
	Received     uint64  `json:"received"`
	QueueDelayMs float64 `json:"queue_delay_ms"`
}

// StreamPoint is one stream count's result.
type StreamPoint struct {
	Streams       int       `json:"streams"`
	Mbps          float64   `json:"mbps"`
	PerStreamMbps []float64 `json:"per_stream_mbps"`
	Retransmits   uint64    `json:"retransmits"`
	RTTms         float64   `json:"rtt_ms"`
}

// StreamScaling compares stream counts in one direction.
type StreamScaling struct {
	Direction              Direction     `json:"direction"`
	Points                 []StreamPoint `json:"points"`
	BestMbps               float64       `json:"best_mbps"`
	SingleStreamEfficiency float64       `json:"single_stream_efficiency"`
}

// BloatPhase is latency during one phase of the bufferbloat test.
type BloatPhase struct {
	Name      string        `json:"name"` // idle, down, up, bidir
	RTT       stats.Summary `json:"rtt_ms"`
	InFlowRTT stats.Summary `json:"in_flow_rtt_ms"`
	Connect   stats.Summary `json:"connect_ms"`
	DownMbps  float64       `json:"down_mbps"`
	UpMbps    float64       `json:"up_mbps"`
	Loss      float64       `json:"probe_loss_pct"`
	Series    []BloatSample `json:"series"`
}

// BloatSample is one latency probe during the bufferbloat test.
type BloatSample struct {
	T     float64 `json:"t"`
	RTTms float64 `json:"rtt_ms"`
	Phase string  `json:"phase"`
}

// Bufferbloat is latency under load.
type Bufferbloat struct {
	Phases         []BloatPhase       `json:"phases"`
	IncreaseMs     map[string]float64 `json:"increase_ms"`
	Grades         map[string]string  `json:"grades"`
	Grade          string             `json:"grade"`
	RPM            float64            `json:"rpm"`
	WorkingLatency float64            `json:"working_latency_ms"`
	Queueing       string             `json:"queueing"` // fifo, flow-queued, unclear
	BufferMs       map[string]float64 `json:"buffer_ms"`
	BufferBytes    map[string]float64 `json:"buffer_bytes"`
	Streams        int                `json:"streams"`
}

// Hop is one traceroute hop.
type Hop struct {
	TTL      int           `json:"ttl"`
	Addr     string        `json:"addr,omitempty"`
	Name     string        `json:"name,omitempty"`
	ASN      string        `json:"asn,omitempty"`
	Org      string        `json:"org,omitempty"`
	Private  bool          `json:"private,omitempty"`
	CGNAT    bool          `json:"cgnat,omitempty"`
	Sent     int           `json:"sent"`
	Received int           `json:"received"`
	LossPct  float64       `json:"loss_pct"`
	RTT      stats.Summary `json:"rtt_ms"`
	Addrs    []string      `json:"addrs,omitempty"` // every responder (ECMP)
	MPLS     []string      `json:"mpls,omitempty"`
	Dest     bool          `json:"dest,omitempty"`
}

// Trace is a traceroute or MTR in one direction.
type Trace struct {
	Direction Direction `json:"direction"`
	From      string    `json:"from"`
	Target    string    `json:"target"`
	Method    string    `json:"method"`
	Cycles    int       `json:"cycles"`
	Hops      []Hop     `json:"hops"`
	Reached   bool      `json:"reached"`
	Note      string    `json:"note,omitempty"`
	// LossFromHop is the first hop whose loss persists to the end (0 if none).
	LossFromHop int `json:"loss_from_hop,omitempty"`
	// RateLimitedHops lists hops whose loss does not persist downstream.
	RateLimitedHops []int  `json:"rate_limited_hops,omitempty"`
	Fingerprint     string `json:"fingerprint"`
}

// PMTUDir is path MTU in one direction.
type PMTUDir struct {
	Direction Direction `json:"direction"`
	MTU       int       `json:"mtu"`
	PTBSeen   bool      `json:"ptb_seen"`
	PTBMTU    int       `json:"ptb_mtu,omitempty"`
	Blackhole bool      `json:"blackhole"`
	LocalMTU  int       `json:"local_mtu"`
	Tested    []PMTUTry `json:"tested"`
	Note      string    `json:"note,omitempty"`
}

// PMTUTry is one probed size.
type PMTUTry struct {
	Size int  `json:"size"`
	OK   bool `json:"ok"`
}

// PMTU covers both directions.
type PMTU struct {
	Up   PMTUDir `json:"up"`
	Down PMTUDir `json:"down"`
}

// DSCPResult is one DSCP value's fate in one direction.
type DSCPResult struct {
	Name     string `json:"name"`
	Sent     int    `json:"sent"`
	Received int    `json:"received"` // -1 if not received
	Status   string `json:"status"`   // preserved, remarked, bleached, lost
}

// Integrity covers middlebox behaviour.
type Integrity struct {
	DSCPUp          []DSCPResult `json:"dscp_up"`
	DSCPDown        []DSCPResult `json:"dscp_down"`
	ECNUp           string       `json:"ecn_up"`
	ECNDown         string       `json:"ecn_down"`
	ClientAdvMSS    uint32       `json:"client_adv_mss"`
	EndpointSeesMSS uint32       `json:"endpoint_snd_mss"`
	EndpointAdvMSS  uint32       `json:"endpoint_adv_mss"`
	ClientSeesMSS   uint32       `json:"client_snd_mss"`
	MSSClampedUp    bool         `json:"mss_clamped_up"`
	MSSClampedDown  bool         `json:"mss_clamped_down"`
	ClientOptions   []string     `json:"client_tcp_options"`
	EndpointOptions []string     `json:"endpoint_tcp_options"`
	StrippedOptions []string     `json:"stripped_tcp_options,omitempty"`
	NAT             bool         `json:"nat"`
	ClientLocal     string       `json:"client_local"`
	ClientObserved  string       `json:"client_observed"`
	PortPreserved   bool         `json:"port_preserved"`
	CGNAT           bool         `json:"cgnat"`
	TTLUp           int          `json:"ttl_received_up"`
	TTLDown         int          `json:"ttl_received_down"`
	HopsUp          int          `json:"hops_up"`
	HopsDown        int          `json:"hops_down"`
}

// VoIPDir is one direction of a simulated call.
type VoIPDir struct {
	Loss        stats.LossStats `json:"loss"`
	JitterMs    float64         `json:"jitter_ms"`
	LateLossPct float64         `json:"late_loss_pct"`
	MOS         float64         `json:"mos"`
	R           float64         `json:"r_factor"`
	Verdict     string          `json:"verdict"`
	// PlayoutMs is the delay queues plus the adaptive jitter buffer added
	// over the idle path (when the endpoint returned packet logs).
	PlayoutMs float64 `json:"playout_ms,omitempty"`
}

// VoIP is a simulated voice call.
type VoIP struct {
	Codec     string  `json:"codec"`
	DurationS float64 `json:"duration_s"`
	OneWayMs  float64 `json:"one_way_ms"`
	Loaded    bool    `json:"loaded"`
	Up        VoIPDir `json:"up"`
	Down      VoIPDir `json:"down"`
	Verdict   string  `json:"verdict"`
}

// SweepStep is one offered rate in a rate sweep.
type SweepStep struct {
	OfferedMbps   float64 `json:"offered_mbps"`
	SentMbps      float64 `json:"sent_mbps"`
	DeliveredMbps float64 `json:"delivered_mbps"`
	LossPct       float64 `json:"loss_pct"`
	QueueP50ms    float64 `json:"queue_p50_ms"`
	QueueP95ms    float64 `json:"queue_p95_ms"`
	JitterMs      float64 `json:"jitter_ms"`
	// NetLossPct excludes packets the receiving host dropped itself;
	// HostLimited marks steps the hosts could not send or receive.
	NetLossPct  float64 `json:"net_loss_pct"`
	HostDropPct float64 `json:"host_drop_pct"`
	HostLimited bool    `json:"host_limited,omitempty"`
	// FirstHalfMbps/SecondHalfMbps reveal burst allowances within a step.
	FirstHalfMbps  float64 `json:"first_half_mbps"`
	SecondHalfMbps float64 `json:"second_half_mbps"`
}

// Sweep is a rate sweep (network response curve) in one direction.
type Sweep struct {
	Direction       Direction   `json:"direction"`
	SizeBytes       int         `json:"size_bytes"`
	StepSeconds     float64     `json:"step_s"`
	Steps           []SweepStep `json:"steps"`
	CapacityMbps    float64     `json:"capacity_mbps"`
	KneeMbps        float64     `json:"knee_mbps"`
	QueueOnsetMbps  float64     `json:"queue_onset_mbps"`
	MaxQueueMs      float64     `json:"max_queue_ms"`
	BufferBytes     float64     `json:"buffer_bytes"`
	Signature       string      `json:"signature"`
	SignatureDetail string      `json:"signature_detail"`
}

// SizePoint is one packet size's result.
type SizePoint struct {
	Size          int     `json:"size"`
	OfferedMbps   float64 `json:"offered_mbps"`
	DeliveredMbps float64 `json:"delivered_mbps"`
	PPS           float64 `json:"pps"`
	LossPct       float64 `json:"loss_pct"`
}

// SizeSweep is a packet-size sweep in one direction.
type SizeSweep struct {
	Direction Direction   `json:"direction"`
	RateMbps  float64     `json:"rate_mbps"`
	Points    []SizePoint `json:"points"`
	MaxPPS    float64     `json:"max_pps"`
}

// CCPoint is one congestion-control algorithm's result.
type CCPoint struct {
	Algorithm   string  `json:"algorithm"`
	Mbps        float64 `json:"mbps"`
	Retransmits uint64  `json:"retransmits"`
	RTTms       float64 `json:"rtt_ms"`
	Error       string  `json:"error,omitempty"`
}

// CCCompare compares congestion-control algorithms (single stream).
type CCCompare struct {
	Direction Direction `json:"direction"`
	Points    []CCPoint `json:"points"`
	Best      string    `json:"best"`
}

// ShortFlow is many short transfers.
type ShortFlow struct {
	Connect     stats.Summary            `json:"connect_ms"`
	Completions map[string]stats.Summary `json:"completion_ms"` // by size label
	Count       int                      `json:"count"`
	Errors      int                      `json:"errors"`
}

// DiscriminationPoint is one protocol/port combination.
type DiscriminationPoint struct {
	Label string  `json:"label"`
	Mbps  float64 `json:"mbps"`
	Error string  `json:"error,omitempty"`
}

// Discrimination compares payload/protocol variants for throttling.
type Discrimination struct {
	Direction Direction             `json:"direction"`
	Points    []DiscriminationPoint `json:"points"`
	Verdict   string                `json:"verdict"`
}

// SQMStep is one shaper setting in the SQM tuner.
type SQMStep struct {
	Percent      float64 `json:"percent"`
	DownMbps     float64 `json:"down_shaped_mbps"`
	UpMbps       float64 `json:"up_shaped_mbps"`
	Qdisc        string  `json:"qdisc"`
	Grade        string  `json:"grade"`
	IncreaseMs   float64 `json:"increase_ms"`
	DownMeasured float64 `json:"down_measured_mbps"`
	UpMeasured   float64 `json:"up_measured_mbps"`
	Error        string  `json:"error,omitempty"`
}

// SQMTuner explores shaping rates against bufferbloat.
type SQMTuner struct {
	Interface     string    `json:"interface"`
	BaselineDown  float64   `json:"baseline_down_mbps"`
	BaselineUp    float64   `json:"baseline_up_mbps"`
	BaselineGrade string    `json:"baseline_grade"`
	Steps         []SQMStep `json:"steps"`
	Recommended   *SQMStep  `json:"recommended,omitempty"`
	TCCommands    []string  `json:"tc_commands,omitempty"`
	Note          string    `json:"note,omitempty"`
}

// Finding is one diagnosis.
type Finding struct {
	ID             string   `json:"id"`
	Severity       string   `json:"severity"` // critical, warning, info, good
	Confidence     string   `json:"confidence"`
	Title          string   `json:"title"`
	Detail         string   `json:"detail"`
	Recommendation string   `json:"recommendation,omitempty"`
	Evidence       []string `json:"evidence,omitempty"` // step IDs
}

// Summary is the headline view of a run.
type Summary struct {
	HealthScore    int                `json:"health_score"`
	ScoreParts     map[string]float64 `json:"score_parts"`
	DownMbps       float64            `json:"down_mbps"`
	UpMbps         float64            `json:"up_mbps"`
	DownSingleMbps float64            `json:"down_single_mbps"`
	UpSingleMbps   float64            `json:"up_single_mbps"`
	IdleRTTms      float64            `json:"idle_rtt_ms"`
	JitterMs       float64            `json:"jitter_ms"`
	LossPct        float64            `json:"loss_pct"`
	BloatGrade     string             `json:"bloat_grade"`
	RPM            float64            `json:"rpm"`
	MOS            float64            `json:"mos"`
	PathMTU        int                `json:"path_mtu"`
	HopsUp         int                `json:"hops_up"`
	HopsDown       int                `json:"hops_down"`
	NAT            bool               `json:"nat"`
	Fingerprint    map[string]string  `json:"fingerprint,omitempty"`
}

// Step returns the first step with the given test and, if dir is non-empty,
// matching direction.
func (r *Report) StepsOf(test string) []Step {
	var out []Step
	for _, s := range r.Steps {
		if s.Test == test && s.Status == "ok" {
			out = append(out, s)
		}
	}
	return out
}

// QoSClass is one marking's latency during the QoS test.
type QoSClass struct {
	Name     string        `json:"name"`
	DSCP     int           `json:"dscp"`
	Idle     stats.Summary `json:"idle_rtt_ms"`
	Loaded   stats.Summary `json:"loaded_rtt_ms"`
	Increase float64       `json:"increase_ms"` // p95 loaded minus idle median
	LossPct  float64       `json:"loss_pct"`
}

// QoS checks whether DSCP priority protects latency under load.
type QoS struct {
	Classes   []QoSClass `json:"classes"`
	DownMbps  float64    `json:"down_mbps"`
	UpMbps    float64    `json:"up_mbps"`
	Effective bool       `json:"priority_effective"`
	Verdict   string     `json:"verdict"`
}

// Mix is the traffic-mix test: simulated application sessions (calls,
// browsing, streaming, transfers) running at once. When the mix includes
// bulk transfers it runs twice, first without them and then with them,
// so the result shows what the transfers do to everything else.
type Mix struct {
	Spec      string     `json:"spec"`
	Scenario  string     `json:"scenario,omitempty"`
	Counts    []MixCount `json:"counts"`
	BaseRTTms float64    `json:"base_rtt_ms"`
	DSCP      bool       `json:"dscp"`
	Phases    []MixPhase `json:"phases"`
	Verdict   string     `json:"verdict"`
	Notes     []string   `json:"notes,omitempty"`
}

// MixCount is how many sessions of one kind the mix ran.
type MixCount struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// MixPhase is one run of the mix.
type MixPhase struct {
	Name          string        `json:"name"` // interactive, with-transfers or all
	Label         string        `json:"label"`
	WithTransfers bool          `json:"with_transfers"`
	DurationS     float64       `json:"duration_s"`
	ProbeRTT      stats.Summary `json:"probe_rtt_ms"`
	Kinds         []MixKind     `json:"kinds"`
	Sessions      []MixSession  `json:"sessions"`
	Verdict       string        `json:"verdict"`
}

// MixKind summarises the sessions of one kind in a phase.
type MixKind struct {
	Kind       string  `json:"kind"`
	Label      string  `json:"label"`
	Sessions   int     `json:"sessions"`
	Good       int     `json:"good"`
	Acceptable int     `json:"acceptable"`
	Poor       int     `json:"poor"`
	Verdict    string  `json:"verdict,omitempty"` // worst session; empty for transfers
	Metric     string  `json:"metric"`            // what Value is, e.g. "worst MOS"
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	Summary    string  `json:"summary"`
}

// MixSession is one simulated application session.
type MixSession struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Verdict string   `json:"verdict,omitempty"`
	Error   string   `json:"error,omitempty"`
	Up      *MixFlow `json:"up,omitempty"`   // real-time sessions: client to endpoint
	Down    *MixFlow `json:"down,omitempty"` // real-time sessions: endpoint to client
	// TCP sessions.
	Mbps     float64        `json:"mbps,omitempty"`
	Times    *stats.Summary `json:"times_ms,omitempty"` // page loads or chunk downloads
	Count    int            `json:"count,omitempty"`
	Errors   int            `json:"errors,omitempty"`
	TargetMs float64        `json:"target_ms,omitempty"` // streaming: play time per chunk
}

// MixFlow is one direction of a real-time session.
type MixFlow struct {
	LossPct    float64 `json:"loss_pct"`
	JitterMs   float64 `json:"jitter_ms"`
	DelayMs    float64 `json:"delay_ms"`     // estimated one-way delay (median)
	AddedP50Ms float64 `json:"added_p50_ms"` // delay added by queues, vs the idle path
	AddedP95Ms float64 `json:"added_p95_ms"`
	LatePct    float64 `json:"late_pct,omitempty"` // voice: arrived after the adaptive jitter buffer played it
	// PlayoutMs is the mean delay queues plus the receiver's adaptive jitter
	// buffer add over the idle path (voice): what the listener hears.
	PlayoutMs float64    `json:"playout_ms,omitempty"`
	MOS       float64    `json:"mos,omitempty"`
	R         float64    `json:"r,omitempty"`
	Frames    *MixFrames `json:"frames,omitempty"` // video
	Verdict   string     `json:"verdict"`
}

// MixFrames summarises how video frames arrived.
type MixFrames struct {
	Frames  int     `json:"frames"`
	OKPct   float64 `json:"ok_pct"`   // complete and on time
	LatePct float64 `json:"late_pct"` // arrived after their playout time
	// RepairedPct lost packets but were repaired by retransmission;
	// LostPct could not be repaired at all.
	RepairedPct float64 `json:"repaired_pct"`
	LostPct     float64 `json:"lost_pct"`
	Freezes     int     `json:"freezes"` // runs of bad frames of 200 ms or more
	FreezeS     float64 `json:"freeze_s"`
	DelayP50Ms  float64 `json:"delay_p50_ms"` // added network delay per frame
	DelayP95Ms  float64 `json:"delay_p95_ms"`
	PlayoutMs   float64 `json:"playout_ms"` // mean delay added by queues plus the jitter buffer
}

// Capacity is how many simultaneous calls of one kind the path carries,
// searched by adding calls until quality drops, on their own and while
// bulk transfers run in both directions.
type Capacity struct {
	Kind           string          `json:"kind"` // voice or video
	Label          string          `json:"label"`
	PerSessionKbps float64         `json:"per_session_kbps"` // each direction, including headers
	StepS          float64         `json:"step_s"`
	Alone          CapacitySearch  `json:"alone"`
	WithTransfers  *CapacitySearch `json:"with_transfers,omitempty"`
}

// CapacitySearch is one capacity search.
type CapacitySearch struct {
	Steps []CapacityStep `json:"steps"`
	// Good is the most sessions with every session good; Acceptable the
	// most with none poor.
	Good       int `json:"good"`
	Acceptable int `json:"acceptable"`
	// Limit is set when the search stopped before quality dropped: the
	// largest count tried, and why ("rate cap" or "session cap").
	Limit       int    `json:"limit,omitempty"`
	LimitReason string `json:"limit_reason,omitempty"`
}

// CapacityStep is one count of simultaneous sessions.
type CapacityStep struct {
	Sessions   int     `json:"sessions"`
	Good       int     `json:"good"`
	Acceptable int     `json:"acceptable"`
	Poor       int     `json:"poor"`
	Worst      float64 `json:"worst"` // worst MOS (voice) or frames-on-time % (video)
	LossPct    float64 `json:"loss_pct"`
	AddedP95Ms float64 `json:"added_p95_ms"`
	Verdict    string  `json:"verdict"`
}

// CDPSettings describes a simulated continuous-data-protection (CDP)
// replication, such as Veeam CDP. Zero values mean the defaults.
type CDPSettings struct {
	RateMbps    float64 `json:"rate_mbps"`              // average changed data on the wire (after compression)
	BurstFactor float64 `json:"burst_factor,omitempty"` // write bursts as a multiple of the average (default 3)
	BurstS      float64 `json:"burst_s,omitempty"`      // length of each burst (default 10 s)
	PeriodS     float64 `json:"period_s,omitempty"`     // time from one burst to the next (default 60 s)
	LimitMbps   float64 `json:"limit_mbps,omitempty"`   // replication throttle (0: none)
	Streams     int     `json:"streams,omitempty"`      // TCP connections (default 4)
	RPOs        float64 `json:"rpo_s,omitempty"`        // target RPO (default 15 s)
	DurationS   float64 `json:"duration_s,omitempty"`   // default 60 s × scale
}

// CDPPoint is one second of a CDP run.
type CDPPoint struct {
	T        float64 `json:"t"`
	LagS     float64 `json:"lag_s"`
	GenMbps  float64 `json:"gen_mbps"`
	SentMbps float64 `json:"sent_mbps"`
}

// CDP is a simulated CDP replication run: changed data produced at a
// steady rate with write bursts, sent to the other site as fast as the path
// allows. Lag is the age of the oldest change not yet sent: the RPO the
// replica would have had at that moment.
type CDP struct {
	Direction      Direction   `json:"direction"` // up: this side is the CDP source
	Settings       CDPSettings `json:"settings"`  // as run, defaults filled in
	AutoRate       bool        `json:"auto_rate,omitempty"`
	BaseMbps       float64     `json:"base_mbps"` // rate between bursts
	PeakMbps       float64     `json:"peak_mbps"` // rate during bursts
	GeneratedMB    float64     `json:"generated_mb"`
	SentMB         float64     `json:"sent_mb"`
	ThroughputMbps float64     `json:"throughput_mbps"`
	// SustainedMbps is what the path carried while changes were waiting
	// (0 when nothing ever waited): the most CDP can count on.
	SustainedMbps float64       `json:"sustained_mbps"`
	Backlogged    bool          `json:"backlogged"`
	MaxLagS       float64       `json:"max_lag_s"`
	P95LagS       float64       `json:"p95_lag_s"`
	MeanLagS      float64       `json:"mean_lag_s"`
	EndLagS       float64       `json:"end_lag_s"`
	WithinRPOPct  float64       `json:"within_rpo_pct"`
	OverRPOS      float64       `json:"over_rpo_s"` // time the lag exceeded the RPO
	RetransPct    float64       `json:"retrans_pct"`
	IdleRTT       stats.Summary `json:"idle_rtt_ms"`
	LoadedRTT     stats.Summary `json:"loaded_rtt_ms"` // latency probes while CDP ran
	Points        []CDPPoint    `json:"points"`
	Verdict       string        `json:"verdict"`
	Notes         []string      `json:"notes,omitempty"`
}
