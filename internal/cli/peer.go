package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/agoodley/pathgauge/internal/analysis"
	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/engine"
	"github.com/agoodley/pathgauge/internal/enrich"
	"github.com/agoodley/pathgauge/internal/logging"
	"github.com/agoodley/pathgauge/internal/report"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/shaping"
)

// NewKey returns a random peer-mode key.
func NewKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "pg_" + base64.RawURLEncoding.EncodeToString(b)
}

func readKey(key, keyFile string, create bool) (string, error) {
	if key != "" {
		return key, nil
	}
	if keyFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(keyFile)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return "", err
	}
	k := NewKey()
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return "", err
	}
	return k, os.WriteFile(keyFile, []byte(k+"\n"), 0o600)
}

func newEndpointCmd() *cobra.Command {
	var listen, key, keyFile, policyFile, logLevel string
	var allowShaping bool
	cmd := &cobra.Command{
		Use:   "endpoint",
		Short: "Run a standalone test endpoint for peer-to-peer testing (no server needed)",
		Long: `Run a standalone PathGauge endpoint. On the other machine, run
"pathgauge test <this-host> --key <key>". The client opens every connection,
so the client may be behind NAT; this endpoint must be reachable on the
listen port over TCP and UDP.

Without --key or --key-file a random key is generated and printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			k, err := readKey(key, keyFile, true)
			if err != nil {
				return err
			}
			generated := false
			if k == "" {
				k, generated = NewKey(), true
			}
			pol := config.DefaultPolicy()
			if policyFile != "" {
				if pol, err = config.LoadPolicy(policyFile); err != nil {
					return err
				}
			}
			logger, err := logging.New(os.Stderr, logging.Options{Level: logLevel})
			if err != nil {
				return err
			}
			var shaper endpoint.Shaper
			if allowShaping && pol.Shaping != config.ShapingNone {
				shaper = shaping.NewManager(logger)
			}
			srv := endpoint.New(endpoint.Options{Listen: listen, Keys: endpoint.StaticKeys{"psk": []byte(k)}, Policy: pol, Logger: logger, Shaper: shaper})
			if err := srv.Listen(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "PathGauge endpoint listening on %s (TCP and UDP)\n", srv.Addr())
			if generated {
				fmt.Fprintf(out, "Key (new for this run): %s\n", k)
			}
			fmt.Fprintf(out, "On the other machine run:\n  pathgauge test <this-host-address> --key %s\n", k)
			if shaper != nil {
				fmt.Fprintln(out, "Shaping requests from clients are allowed (leases expire automatically).")
			}
			return srv.Serve(cmd.Context())
		},
	}
	cmd.Flags().StringVarP(&listen, "listen", "l", fmt.Sprintf(":%d", config.DefaultDataPort), "address to listen on (TCP and UDP)")
	cmd.Flags().StringVar(&key, "key", "", "shared key clients must present")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "read the key from this file (created with a new key if missing)")
	cmd.Flags().StringVar(&policyFile, "policy", "", "safety policy file (default: built-in policy)")
	cmd.Flags().BoolVar(&allowShaping, "allow-shaping", false, "let clients request time-limited shaping on this host (needs CAP_NET_ADMIN)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level")
	return cmd
}

type testFlags struct {
	key, keyFile, profile, jsonOut, htmlOut, clientName, endpointName, title string
	tests, skip                                                              []string
	scale, maxRate, expectDown, expectUp                                     float64
	streams, traceCycles                                                     int
	noResolve, quiet, noSave, allowShaping, noDSCP                           bool
	shapeIface, mix                                                          string
}

func newTestCmd() *cobra.Command {
	var f testFlags
	cmd := &cobra.Command{
		Use:   "test HOST[:PORT]",
		Short: "Test the path to a PathGauge endpoint and report the results",
		Long: `Run a test profile against a PathGauge endpoint (started with
"pathgauge endpoint" or a PathGauge server) and print a summary.

Profiles: quick (about 1 minute), standard (about 4 minutes), full (about
12 minutes, includes sweeps). Throughput tests fill the link and use a lot of
data: roughly 5 GB for quick and 10-15 GB for standard at 1 Gbit/s.

Results are saved as JSON (see --json); render them again later with
"pathgauge report".`,
		Example: `  pathgauge test 203.0.113.10 --key pg_xxx
  pathgauge test 203.0.113.10 --key pg_xxx --profile full --html report.html
  pathgauge test 203.0.113.10 --key pg_xxx --tests latency,bufferbloat`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTest(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.key, "key", os.Getenv("PATHGAUGE_KEY"), "endpoint key (or PATHGAUGE_KEY)")
	fl.StringVar(&f.keyFile, "key-file", "", "read the key from a file")
	fl.StringVarP(&f.profile, "profile", "p", "standard", "quick, standard or full")
	fl.StringSliceVar(&f.tests, "tests", nil, "run only these tests (comma-separated): "+strings.Join(engine.AllTests, ","))
	fl.StringSliceVar(&f.skip, "skip", nil, "skip these tests")
	fl.Float64Var(&f.scale, "scale", 1, "multiply every test duration (e.g. 0.5 for a faster run)")
	fl.Float64Var(&f.maxRate, "max-rate", 0, "cap UDP tests and sweeps at this many Mbit/s")
	fl.IntVar(&f.streams, "streams", 8, "parallel TCP streams for multi-stream tests")
	fl.IntVar(&f.traceCycles, "trace-cycles", 0, "traceroute cycles per hop (default 3; 20 for mtr)")
	fl.BoolVar(&f.noResolve, "no-resolve", false, "skip reverse DNS for hops")
	fl.StringVar(&f.jsonOut, "json", "", "write results JSON to this file (default: saved under ~/.local/share/pathgauge/results)")
	fl.BoolVar(&f.noSave, "no-save", false, "do not save results JSON to the default location (--json still writes)")
	fl.StringVar(&f.htmlOut, "html", "", "write an HTML report to this file")
	fl.StringVar(&f.title, "title", "", "HTML report title")
	fl.StringVar(&f.clientName, "client-name", "", "name for this end in reports (default: hostname)")
	fl.StringVar(&f.endpointName, "endpoint-name", "", "name for the endpoint in reports (default: its hostname)")
	fl.Float64Var(&f.expectDown, "expect-down", 0, "expected download Mbit/s (adds SLA findings)")
	fl.Float64Var(&f.expectUp, "expect-up", 0, "expected upload Mbit/s")
	fl.BoolVarP(&f.quiet, "quiet", "q", false, "no live progress")
	fl.BoolVar(&f.allowShaping, "allow-shaping", false, "allow the sqm test to shape this host's interface temporarily (root or CAP_NET_ADMIN)")
	fl.StringVar(&f.shapeIface, "shape-interface", "", "interface for the sqm test (default: the route to the endpoint)")
	fl.StringVar(&f.mix, "mix", "", "traffic mix for the mix test: a scenario ("+strings.Join(mixScenarioList(), ", ")+") or kind:count,... (default "+engine.DefaultMix+")")
	fl.BoolVar(&f.noDSCP, "no-dscp", false, "send simulated calls without DSCP markings (mix and capacity tests)")
	return cmd
}

func runTest(ctx context.Context, out, errOut io.Writer, host string, f testFlags) error {
	key, err := readKey(f.key, f.keyFile, false)
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("an endpoint key is required (--key, --key-file or PATHGAUGE_KEY)")
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	c, err := engine.Dial(dctx, host, "psk", []byte(key))
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()
	tty := isTTY(out) && !f.quiet
	opts := engine.Options{
		Profile: f.profile, Tests: f.tests, Skip: f.skip, Scale: f.scale, MaxRateMbps: f.maxRate,
		TraceCycles: f.traceCycles, Resolve: !f.noResolve, Streams: f.streams,
		Labels: results.LinkLabels{Client: f.clientName, Endpoint: f.endpointName},
		Mix:    f.mix, NoDSCP: f.noDSCP,
	}
	if f.mix != "" {
		if _, _, err := engine.ParseMix(f.mix); err != nil {
			return err
		}
		if len(f.tests) == 0 {
			opts.Tests = []string{"info", "mix"} // --mix alone runs just the mix
		}
	}
	if f.allowShaping {
		opts.SQM = shaping.NewTuner(slog.Default(), f.shapeIface)
	}
	if db := enrich.OpenDefault(""); db != nil {
		defer db.Close()
		opts.Annotate = db.Lookup
	}
	if !f.quiet {
		opts.Progress = progressPrinter(out, tty)
	}
	fmt.Fprintf(out, "Connected to %s (endpoint %s); this host appears as %s\n", c.Addr, c.EndpointSoftware, c.Observed)
	rep, err := engine.Run(ctx, c, opts)
	if err != nil {
		return err
	}
	if opts.Annotate != nil {
		rep.Notes = append(rep.Notes, enrich.Attribution)
	}
	analysis.Analyze(rep, analysis.Expectations{DownMbps: f.expectDown, UpMbps: f.expectUp})
	if err := report.WriteText(out, rep); err != nil {
		return err
	}
	if !f.noSave || f.jsonOut != "" {
		path := f.jsonOut
		if path == "" {
			path = defaultResultPath(rep)
		}
		if err := saveJSON(path, rep); err != nil {
			fmt.Fprintf(errOut, "warning: could not save results: %v\n", err)
		} else {
			fmt.Fprintf(out, "\nResults saved to %s\n", path)
		}
	}
	if f.htmlOut != "" {
		if err := writeHTMLFile(f.htmlOut, rep, report.Options{Title: f.title}); err != nil {
			return err
		}
		fmt.Fprintf(out, "HTML report written to %s\n", f.htmlOut)
	}
	return nil
}

func defaultResultPath(r *results.Report) string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "pathgauge", "results", fmt.Sprintf("%s-%s.json", r.StartedAt.Format("20060102-150405"), r.ID[:6]))
}

func saveJSON(path string, r *results.Report) error {
	// Results contain addresses and host details: keep them private.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func writeHTMLFile(path string, r *results.Report, o report.Options) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := report.WriteHTML(fh, r, o); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

func isTTY(w io.Writer) bool {
	fh, ok := w.(*os.File)
	return ok && term.IsTerminal(int(fh.Fd()))
}

// progressPrinter prints step progress; on a terminal it also shows a
// live-updating line.
func progressPrinter(out io.Writer, tty bool) func(engine.Event) {
	lastLive := time.Time{}
	return func(e engine.Event) {
		switch e.Type {
		case "step_start":
			fmt.Fprintf(out, "[%d/%d] %s …\n", e.Index, e.Total, e.Title)
		case "progress":
			if !tty || time.Since(lastLive) < 250*time.Millisecond {
				return
			}
			lastLive = time.Now()
			if m, ok := e.Data.(map[string]any); ok {
				switch m["kind"] {
				case "throughput":
					fmt.Fprintf(out, "\r\033[K      %v  t=%5.1fs  %8.1f Mbit/s", m["direction"], m["t"], m["mbps"])
				case "latency":
					fmt.Fprintf(out, "\r\033[K      %-6v  t=%5.1fs  RTT %7.2f ms", m["phase"], m["t"], m["rtt_ms"])
				case "sweep":
					if s, ok := m["step"].(results.SweepStep); ok {
						fmt.Fprintf(out, "\r\033[K      offered %7.1f → delivered %7.1f Mbit/s, loss %5.2f%%", s.OfferedMbps, s.DeliveredMbps, s.LossPct)
					}
				}
			}
		case "step_done":
			if tty {
				fmt.Fprint(out, "\r\033[K")
			}
			mark := "✓"
			if e.Status == "error" {
				mark = "✖"
			}
			msg := ""
			if e.Error != "" {
				msg = ": " + e.Error
			}
			fmt.Fprintf(out, "      %s %.1fs%s\n", mark, e.Seconds, msg)
		}
	}
}

func newReportCmd() *cobra.Command {
	var htmlOut, title, tmpl, org, notes string
	var text, reanalyse bool
	var expectDown, expectUp float64
	cmd := &cobra.Command{
		Use:   "report RESULTS.json",
		Short: "Render saved results as an HTML report or terminal summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var r results.Report
			if err := json.Unmarshal(b, &r); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if reanalyse {
				analysis.Analyze(&r, analysis.Expectations{DownMbps: expectDown, UpMbps: expectUp})
			}
			if htmlOut == "" || text {
				if err := report.WriteText(cmd.OutOrStdout(), &r); err != nil {
					return err
				}
			}
			if htmlOut != "" {
				if err := writeHTMLFile(htmlOut, &r, report.Options{Title: title, Template: tmpl, Org: org, Notes: notes}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "HTML report written to %s\n", htmlOut)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&htmlOut, "html", "", "write an HTML report to this file")
	cmd.Flags().BoolVar(&text, "text", false, "also print the terminal summary")
	cmd.Flags().StringVar(&title, "title", "", "report title")
	cmd.Flags().StringVar(&tmpl, "template", "technical", "technical or executive")
	cmd.Flags().StringVar(&org, "org", "", "organisation name for the header")
	cmd.Flags().StringVar(&notes, "notes", "", "notes to include")
	cmd.Flags().BoolVar(&reanalyse, "reanalyse", false, "recompute findings and the health score with this version's rules")
	cmd.Flags().Float64Var(&expectDown, "expect-down", 0, "expected download Mbit/s (with --reanalyse)")
	cmd.Flags().Float64Var(&expectUp, "expect-up", 0, "expected upload Mbit/s (with --reanalyse)")
	return cmd
}

func newEnrichCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Manage the offline IP-to-network-operator database used to label traceroute hops",
	}
	var path string
	update := &cobra.Command{
		Use:   "update",
		Short: "Download the latest DB-IP ASN Lite database (CC BY 4.0)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				path = enrich.DefaultPaths("")[0]
				if os.Geteuid() == 0 {
					path = "/var/lib/pathgauge/" + enrich.FileName
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			url, err := enrich.Update(ctx, path)
			if err != nil {
				return err
			}
			if os.Geteuid() == 0 {
				chownData(filepath.Dir(path))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed %s\nfrom %s\n%s\n", path, url, enrich.Attribution)
			return nil
		},
	}
	update.Flags().StringVar(&path, "path", "", "where to install the database (default: ~/.local/share/pathgauge, or /var/lib/pathgauge as root)")
	lookup := &cobra.Command{
		Use:   "lookup ADDRESS...",
		Short: "Show the network operator of addresses",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db := enrich.OpenDefault("")
			if db == nil {
				return errors.New("no database found: run 'pathgauge enrich update' first")
			}
			defer db.Close()
			for _, a := range args {
				asn, org := db.Lookup(a)
				fmt.Fprintf(cmd.OutOrStdout(), "%-40s %-10s %s\n", a, asn, org)
			}
			return nil
		},
	}
	cmd.AddCommand(update, lookup)
	return cmd
}

func mixScenarioList() []string {
	var n []string
	for k := range engine.MixScenarios {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}
