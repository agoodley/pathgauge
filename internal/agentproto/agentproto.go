// Package agentproto defines the messages exchanged between the server and
// agents over the control WebSocket (JSON, one message per frame).
package agentproto

import (
	"encoding/json"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/store"
)

// Message types.
const (
	// agent → server
	TypeHello   = "hello"
	TypeEvent   = "event"
	TypeResult  = "result"
	TypeFailed  = "failed"
	TypeMonitor = "monitor"
	TypePong    = "pong"
	// server → agent
	TypeWelcome = "welcome"
	TypeRun     = "run"
	TypeCancel  = "cancel"
	TypePing    = "ping"
	TypeConfig  = "config"
)

// Msg is the envelope for every control message.
type Msg struct {
	Type  string          `json:"type"`
	RunID string          `json:"run_id,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Hello is the agent's first message.
type Hello struct {
	Version string          `json:"version"`
	Host    json.RawMessage `json:"host"`
	Policy  json.RawMessage `json:"policy"`
	// RunID is the run this agent is still working on: a test keeps going
	// when the control connection drops. The server reattaches it, or
	// answers with a cancel if it has given up on that run.
	RunID string `json:"run_id,omitempty"`
}

// Welcome tells the agent how to reach the server's data plane.
type Welcome struct {
	DataPort        int    `json:"data_port"`
	DataHost        string `json:"data_host,omitempty"` // empty: same host as the server URL
	MonitorEnabled  bool   `json:"monitor_enabled"`
	MonitorInterval int    `json:"monitor_interval"`
	ServerVersion   string `json:"server_version"`
}

// RunRequest starts a test run on the agent.
type RunRequest struct {
	Profile     string   `json:"profile"`
	Tests       []string `json:"tests,omitempty"`
	Skip        []string `json:"skip,omitempty"`
	Scale       float64  `json:"scale,omitempty"`
	MaxRateMbps float64  `json:"max_rate_mbps,omitempty"`
	Streams     int      `json:"streams,omitempty"`
	TraceCycles int      `json:"trace_cycles,omitempty"`
	Mix         string   `json:"mix,omitempty"`     // traffic mix for the mix test
	NoDSCP      bool     `json:"no_dscp,omitempty"` // send simulated calls unmarked
	// CDP configures the cdp-up/cdp-down tests.
	CDP *results.CDPSettings `json:"cdp,omitempty"`
	// Groups ("a+b" entries in Tests): how long each grouped test runs,
	// and whether to skip running each on its own first.
	GroupDurationS float64 `json:"group_duration_s,omitempty"`
	NoBaseline     bool    `json:"no_baseline,omitempty"`
	ClientName     string  `json:"client_name"`
	ServerName     string  `json:"server_name"`
}

// Monitor carries monitoring samples.
type Monitor struct {
	Samples []store.Sample `json:"samples"`
}

// Config updates the agent's monitoring settings.
type Config struct {
	MonitorEnabled  bool `json:"monitor_enabled"`
	MonitorInterval int  `json:"monitor_interval"`
}

// EnrollRequest is sent by `pathgauge agent join`.
type EnrollRequest struct {
	Token   string          `json:"token"`
	Name    string          `json:"name"`
	CSR     string          `json:"csr"`
	Version string          `json:"version"`
	Host    json.RawMessage `json:"host"`
}

// EnrollResponse returns the agent's identity.
type EnrollResponse struct {
	AgentID string `json:"agent_id"`
	Cert    string `json:"cert"`
	CA      string `json:"ca"`
	DataKey []byte `json:"data_key"`
}
