// Package config loads and validates the server, agent and agent-policy
// configuration files (YAML, normally under /etc/pathgauge).
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/agoodley/pathgauge/internal/logging"
)

// Defaults shared by packaging, documentation and code.
const (
	DefaultConfigDir   = "/etc/pathgauge"
	DefaultDataDir     = "/var/lib/pathgauge"
	DefaultControlPort = 7443 // HTTPS UI, REST API, agent control (gRPC)
	DefaultDataPort    = 7400 // test traffic, TCP and UDP

	DefaultServerConfig = DefaultConfigDir + "/server.yaml"
	DefaultAgentConfig  = DefaultConfigDir + "/agent.yaml"
	DefaultPolicyFile   = DefaultConfigDir + "/policy.yaml"
)

// Log configures logging for a long-running role.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// DataPlane configures the test-traffic endpoint.
type DataPlane struct {
	// Listen is host:port for both TCP and UDP test traffic.
	Listen string `yaml:"listen"`
	// PeerKey, if set on a server, also lets "pathgauge test --key" clients
	// test against the server's endpoint without enrolling.
	PeerKey string `yaml:"peer_key"`
	// Advertise is the host agents use to reach the data plane (default:
	// the host in the agent's server URL).
	Advertise string `yaml:"advertise"`
}

// Server is /etc/pathgauge/server.yaml.
type Server struct {
	// Listen is host:port for the dashboard, REST API and agent control.
	Listen    string    `yaml:"listen"`
	DataDir   string    `yaml:"data_dir"`
	DataPlane DataPlane `yaml:"data_plane"`
	Log       Log       `yaml:"log"`
	// Names are extra DNS names or IPs for the server's TLS certificate
	// (the hostname and local addresses are always included).
	Names []string `yaml:"names"`
	// RetentionDays keeps monitoring samples this long.
	RetentionDays int `yaml:"retention_days"`
	// Policy is the safety policy of the server's own endpoint.
	PolicyFile string `yaml:"policy_file"`
}

// Agent is /etc/pathgauge/agent.yaml.
type Agent struct {
	// Name identifies the agent in the dashboard. Empty means the hostname.
	Name string `yaml:"name"`
	// ServerURL is written by `pathgauge agent join`.
	ServerURL  string    `yaml:"server_url"`
	DataDir    string    `yaml:"data_dir"`
	DataPlane  DataPlane `yaml:"data_plane"`
	PolicyFile string    `yaml:"policy_file"`
	Log        Log       `yaml:"log"`
}

// DefaultServer returns the built-in server configuration.
func DefaultServer() Server {
	return Server{
		Listen:        ":" + strconv.Itoa(DefaultControlPort),
		DataDir:       DefaultDataDir,
		DataPlane:     DataPlane{Listen: ":" + strconv.Itoa(DefaultDataPort)},
		Log:           Log{Level: "info", Format: "auto"},
		RetentionDays: 90,
	}
}

// DefaultAgent returns the built-in agent configuration.
func DefaultAgent() Agent {
	return Agent{
		DataDir:    DefaultDataDir,
		DataPlane:  DataPlane{Listen: ":" + strconv.Itoa(DefaultDataPort)},
		PolicyFile: DefaultPolicyFile,
		Log:        Log{Level: "info", Format: "auto"},
	}
}

// LoadServer reads a server configuration file over the defaults.
// An empty path returns the defaults.
func LoadServer(path string) (Server, error) {
	cfg := DefaultServer()
	if err := decodeFile(path, &cfg); err != nil {
		return Server{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Server{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// LoadAgent reads an agent configuration file over the defaults.
// An empty path returns the defaults.
func LoadAgent(path string) (Agent, error) {
	cfg := DefaultAgent()
	if err := decodeFile(path, &cfg); err != nil {
		return Agent{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Agent{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports the first configuration error.
func (s Server) Validate() error {
	if err := validateListen("listen", s.Listen); err != nil {
		return err
	}
	if err := validateListen("data_plane.listen", s.DataPlane.Listen); err != nil {
		return err
	}
	if err := validateDataDir(s.DataDir); err != nil {
		return err
	}
	return validateLog(s.Log)
}

// Validate reports the first configuration error.
func (a Agent) Validate() error {
	if err := validateListen("data_plane.listen", a.DataPlane.Listen); err != nil {
		return err
	}
	if err := validateDataDir(a.DataDir); err != nil {
		return err
	}
	if a.PolicyFile != "" && !filepath.IsAbs(a.PolicyFile) {
		return fmt.Errorf("policy_file: %q must be an absolute path", a.PolicyFile)
	}
	return validateLog(a.Log)
}

func decodeFile(path string, out any) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only: nothing to flush

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // reject typos instead of silently ignoring them
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func validateListen(field, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %q is not host:port: %w", field, addr, err)
	}
	if host != "" && net.ParseIP(host) == nil && !isHostname(host) {
		return fmt.Errorf("%s: invalid host %q", field, host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 { // 0 = any free port (tests)
		return fmt.Errorf("%s: invalid port %q", field, port)
	}
	return nil
}

func isHostname(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func validateDataDir(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("data_dir: %q must be an absolute path", dir)
	}
	return nil
}

func validateLog(l Log) error {
	if _, err := logging.ParseLevel(l.Level); err != nil {
		return fmt.Errorf("log.level: %w", err)
	}
	switch l.Format {
	case "", "auto", "text", "json":
		return nil
	default:
		return fmt.Errorf("log.format: %q is not auto, text or json", l.Format)
	}
}
