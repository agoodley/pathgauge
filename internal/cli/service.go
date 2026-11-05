package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/agoodley/pathgauge/internal/agent"
	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/logging"
	"github.com/agoodley/pathgauge/internal/server"
	"github.com/agoodley/pathgauge/internal/tlsprofile"
	"github.com/agoodley/pathgauge/internal/version"
)

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run and administer the PathGauge server (dashboard, API, orchestration)",
	}
	var cfgPath string
	cmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", config.DefaultServerConfig, "server configuration file (empty for built-in defaults)")

	var checkOnly bool
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the server in the foreground (used by pathgauge-server.service)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadServer(cfgPath)
			if err != nil {
				return err
			}
			if checkOnly {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "configuration (%s): OK\n", displayPath(cfgPath))
				return err
			}
			logger, err := logging.New(os.Stderr, logging.Options{Level: cfg.Log.Level, Format: cfg.Log.Format})
			if err != nil {
				return err
			}
			logger.Info("server starting", "version", version.Get().Version, "listen", cfg.Listen, "data_plane", cfg.DataPlane.Listen, "data_dir", cfg.DataDir)
			srv, err := server.Open(cfg, logger)
			if err != nil {
				return err
			}
			defer srv.Close()
			return srv.Run(cmd.Context())
		},
	}
	run.Flags().BoolVar(&checkOnly, "check-config", false, "validate the configuration and exit")

	var admin, passwordFile string
	var enable bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Prepare the server: create the first administrator and certificates",
		Long: `Prepare a new server: create the database, the internal certificate
authority and TLS certificate, and the first administrator account.
With --enable it also enables and starts pathgauge-server.service.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadServer(cfgPath)
			if err != nil {
				return err
			}
			srv, err := server.Open(cfg, discardLogger())
			if err != nil {
				return err
			}
			defer srv.Close()
			out := cmd.OutOrStdout()
			ctx := cmd.Context()
			if srv.Store().CountUsers(ctx) == 0 {
				if admin == "" {
					admin = "admin"
				}
				pw, err := readPassword(passwordFile, "Password for "+admin+": ")
				if err != nil {
					return err
				}
				hash, err := server.HashPassword(pw)
				if err != nil {
					return err
				}
				if _, err := srv.Store().CreateUser(ctx, admin, hash, "admin"); err != nil {
					return err
				}
				srv.Store().Audit(ctx, "cli", "setup.admin", admin, "created by server init")
				fmt.Fprintf(out, "Administrator %q created.\n", admin)
			} else {
				fmt.Fprintln(out, "Users already exist; no administrator created.")
			}
			chownData(cfg.DataDir)
			fmt.Fprintf(out, "CA fingerprint: %s\n", srv.CA().Fingerprint())
			if enable {
				if err := systemctl("enable", "--now", "pathgauge-server.service"); err != nil {
					return err
				}
				fmt.Fprintln(out, "pathgauge-server.service enabled and started.")
			}
			fmt.Fprintf(out, "Dashboard: https://<this-host>%s  (trust the CA from /ca.crt to avoid browser warnings)\n", portOf(cfg.Listen))
			fmt.Fprintln(out, "Create an agent join token with: sudo pathgauge server join-token")
			return nil
		},
	}
	initCmd.Flags().StringVar(&admin, "admin", "", "administrator username (default admin)")
	initCmd.Flags().StringVar(&passwordFile, "password-file", "", "read the administrator password from a file instead of prompting")
	initCmd.Flags().BoolVar(&enable, "enable", false, "enable and start pathgauge-server.service")

	var hours float64
	var note, serverURL string
	tokenCmd := &cobra.Command{
		Use:   "join-token",
		Short: "Create a single-use token for enrolling an agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadServer(cfgPath)
			if err != nil {
				return err
			}
			srv, err := server.Open(cfg, discardLogger())
			if err != nil {
				return err
			}
			defer srv.Close()
			tok, err := srv.CreateJoinToken(cmd.Context(), "cli", note, time.Duration(hours*float64(time.Hour)))
			if err != nil {
				return err
			}
			chownData(cfg.DataDir)
			if serverURL == "" {
				serverURL = "https://<this-server>" + portOf(cfg.Listen)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Join token (single use, expires in %.0f h):\n  %s\n\nOn the agent machine run:\n  sudo pathgauge agent join %s --token %s --name <agent-name>\n", hours, tok, serverURL, tok)
			return nil
		},
	}
	tokenCmd.Flags().Float64Var(&hours, "hours", 24, "hours until the token expires")
	tokenCmd.Flags().StringVar(&note, "note", "", "note stored with the token")
	tokenCmd.Flags().StringVar(&serverURL, "url", "", "server URL to show in the join command")

	resetCmd := &cobra.Command{
		Use:   "reset-password USER",
		Short: "Set a user's password (and make sure the account is enabled)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadServer(cfgPath)
			if err != nil {
				return err
			}
			srv, err := server.Open(cfg, discardLogger())
			if err != nil {
				return err
			}
			defer srv.Close()
			u, err := srv.Store().UserByName(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("user %q: %w", args[0], err)
			}
			pw, err := readPassword(passwordFile, "New password for "+u.Username+": ")
			if err != nil {
				return err
			}
			hash, err := server.HashPassword(pw)
			if err != nil {
				return err
			}
			if err := srv.Store().UpdateUser(cmd.Context(), u.ID, u.Role, hash, false); err != nil {
				return err
			}
			srv.Store().Audit(cmd.Context(), "cli", "user.reset_password", u.Username, "")
			fmt.Fprintln(cmd.OutOrStdout(), "Password updated; existing sessions were signed out.")
			return nil
		},
	}
	resetCmd.Flags().StringVar(&passwordFile, "password-file", "", "read the password from a file instead of prompting")

	cmd.AddCommand(run, initCmd, tokenCmd, resetCmd)
	return cmd
}

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Enrol and run the PathGauge agent (a test endpoint controlled by a server)",
	}
	var cfgPath string
	cmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", config.DefaultAgentConfig, "agent configuration file (empty for built-in defaults)")
	load := func() (config.Agent, config.Policy, error) {
		cfg, err := config.LoadAgent(cfgPath)
		if err != nil {
			return cfg, config.Policy{}, err
		}
		if cfgPath == "" {
			cfg.PolicyFile = ""
		}
		pol, err := config.LoadPolicy(cfg.PolicyFile)
		return cfg, pol, err
	}
	var checkOnly bool
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground (used by pathgauge-agent.service)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, pol, err := load()
			if err != nil {
				return err
			}
			if checkOnly {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "configuration (%s): OK\npolicy (%s): OK\n", displayPath(cfgPath), displayPath(cfg.PolicyFile))
				return err
			}
			logger, err := logging.New(os.Stderr, logging.Options{Level: cfg.Log.Level, Format: cfg.Log.Format})
			if err != nil {
				return err
			}
			a, err := agent.New(cfg, pol, logger)
			if err != nil {
				return err
			}
			return a.Run(cmd.Context())
		},
	}
	run.Flags().BoolVar(&checkOnly, "check-config", false, "validate the configuration and policy, then exit")

	var token, name, tlsProfile string
	var enable, debug bool
	join := &cobra.Command{
		Use:   "join https://SERVER:7443 --token TOKEN",
		Short: "Enrol this machine as an agent of a PathGauge server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := load()
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("--token is required (create one in the dashboard or with 'pathgauge server join-token')")
			}
			if name == "" {
				name = cfg.Name
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			o := agent.JoinOptions{URL: args[0], Token: token, Name: name, DataDir: cfg.DataDir, TLSProfile: tlsProfile}
			if debug {
				o.Debug = cmd.ErrOrStderr()
			}
			id, err := agent.JoinWith(ctx, o)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Enrolled as %q (%s) with %s.\n", id.Name, id.AgentID, id.ServerURL)
			switch {
			case id.TLSProfile != "" && tlsProfile == tlsprofile.Auto:
				fmt.Fprintf(cmd.OutOrStdout(), "Using TLS profile %q (the default TLS hello got no answer on this path); the agent service will use it too.\n", id.TLSProfile)
			case id.TLSProfile != "":
				fmt.Fprintf(cmd.OutOrStdout(), "Using TLS profile %q; the agent service will use it too.\n", id.TLSProfile)
			}
			if enable {
				if err := systemctl("enable", "--now", "pathgauge-agent.service"); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "pathgauge-agent.service enabled and started.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Start it with: sudo systemctl enable --now pathgauge-agent")
			}
			return nil
		},
	}
	join.Flags().StringVar(&token, "token", "", "join token from the server")
	join.Flags().StringVar(&name, "name", "", "agent name shown in the dashboard (default: hostname)")
	join.Flags().BoolVar(&enable, "enable", false, "enable and start pathgauge-agent.service after joining")
	join.Flags().StringVar(&tlsProfile, "tls-profile", tlsprofile.Auto, "how to open TLS connections: auto (try each until one gets an answer), "+strings.Join(tlsprofile.Names(), ", "))
	join.Flags().BoolVar(&debug, "debug", false, "print each step of the connection (TCP, TLS, request, answer)")

	var dataPort int
	diagnose := &cobra.Command{
		Use:   "diagnose https://SERVER:7443",
		Short: "Check the path to a server step by step: TCP, each TLS profile, HTTPS and the test-traffic port",
		Long: `Check the path to a PathGauge server step by step, without enrolling and
without sending any secret: name resolution, the TCP connection, a TLS
handshake with each TLS profile, an HTTPS request, and the test-traffic port.

Use it when 'pathgauge agent join' fails or hangs. Some firewalls, IPSs and
endpoint-security products silently drop TLS hellos they don't recognise;
this shows which TLS profiles get through.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			return agent.Diagnose(ctx, cmd.OutOrStdout(), args[0], dataPort)
		},
	}
	diagnose.Flags().IntVar(&dataPort, "data-port", config.DefaultDataPort, "the server's test-traffic port (0 to skip)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show this agent's enrollment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, pol, err := load()
			if err != nil {
				return err
			}
			id, err := agent.LoadIdentity(cfg.DataDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Agent:   %s (%s)\nServer:  %s\nPolicy:  max %d Mbit/s, shaping %s, tests %v, hours %v\n",
				id.Name, id.AgentID, id.ServerURL, pol.MaxRateMbps, pol.Shaping, orAll(pol.AllowedTests), orAll(pol.AllowedHours))
			return nil
		},
	}
	cmd.AddCommand(run, join, status, diagnose)
	return cmd
}

func orAll(s []string) any {
	if len(s) == 0 {
		return "all"
	}
	return s
}

func displayPath(p string) string {
	if p == "" {
		return "built-in defaults"
	}
	return p
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ""
}

func readPassword(file, prompt string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		r := bufio.NewReader(os.Stdin)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password given (use --password-file or a terminal)")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat: ")
	b2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(b) != string(b2) {
		return "", errors.New("passwords do not match")
	}
	return string(b), nil
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// chownData hands the data directory to the service user when an admin
// command runs as root, so the service can still open the database.
func chownData(dir string) {
	if os.Geteuid() != 0 {
		return
	}
	_ = exec.Command("chown", "-R", "pathgauge:pathgauge", dir).Run()
}
