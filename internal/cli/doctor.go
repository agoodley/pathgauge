package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/agoodley/pathgauge/internal/hostinfo"
)

func newDoctorCmd() *cobra.Command {
	var asJSON, strict bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check this host is ready for network testing",
		Long: `Check this host's readiness for network testing: privileges, kernel
features, congestion-control algorithms, queueing disciplines for shaping,
TCP/UDP buffer limits, clock synchronisation and network interfaces.

Each problem is reported with its impact on results and how to fix it.
Run it on both ends of the path you want to test. Results depend on the
user running it: the systemd services hold CAP_NET_RAW and CAP_NET_ADMIN,
so run it with sudo to see what the services will see.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := hostinfo.Collect(hostinfo.SystemSources())
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			} else if err := hostinfo.WriteText(out, report); err != nil {
				return err
			}
			if strict {
				if n := report.Counts(); n[hostinfo.StatusWarn]+n[hostinfo.StatusMissing] > 0 {
					return exitError{code: 2, err: fmt.Errorf("%d warnings, %d missing", n[hostinfo.StatusWarn], n[hostinfo.StatusMissing])}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit with status 2 if any check warns or is missing")
	return cmd
}
