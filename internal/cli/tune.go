package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/agoodley/pathgauge/internal/hostinfo"
)

const tuneFile = "/etc/sysctl.d/90-pathgauge.conf"

type sysctlAdvice struct {
	key, current, want, why string
}

// tuneAdvice compares current sysctls with what a single TCP stream needs
// to reach rateMbps at rttMs (2x the bandwidth-delay product of buffer).
func tuneAdvice(rateMbps, rttMs float64, bbr bool, read func(string) string) []sysctlAdvice {
	bdp := rateMbps * 1e6 / 8 * rttMs / 1000
	want := int64(16 << 20)
	for float64(want) < 2*bdp {
		want <<= 1
	}
	var out []sysctlAdvice
	maxOf := func(v string) int64 {
		f := strings.Fields(v)
		if len(f) == 0 {
			return 0
		}
		n, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
		return n
	}
	for _, k := range []string{"net.core.rmem_max", "net.core.wmem_max"} {
		if cur := read(k); maxOf(cur) < want {
			out = append(out, sysctlAdvice{k, cur, strconv.FormatInt(want, 10), "allows large socket buffers for fast, long paths"})
		}
	}
	if cur := read("net.ipv4.tcp_rmem"); maxOf(cur) < want {
		out = append(out, sysctlAdvice{"net.ipv4.tcp_rmem", cur, fmt.Sprintf("4096 131072 %d", want), fmt.Sprintf("receive window for %.0f Mbit/s at %.0f ms (BDP %s)", rateMbps, rttMs, hostinfo.FormatBytes(int64(bdp)))})
	}
	if cur := read("net.ipv4.tcp_wmem"); maxOf(cur) < want {
		out = append(out, sysctlAdvice{"net.ipv4.tcp_wmem", cur, fmt.Sprintf("4096 16384 %d", want), "send buffer for the same"})
	}
	if cur := read("net.ipv4.tcp_mtu_probing"); cur != "1" && cur != "2" {
		out = append(out, sysctlAdvice{"net.ipv4.tcp_mtu_probing", cur, "1", "lets TCP recover from path-MTU black holes"})
	}
	if bbr {
		if cur := read("net.core.default_qdisc"); cur != "fq" {
			out = append(out, sysctlAdvice{"net.core.default_qdisc", cur, "fq", "pacing for BBR"})
		}
		if cur := read("net.ipv4.tcp_congestion_control"); cur != "bbr" {
			out = append(out, sysctlAdvice{"net.ipv4.tcp_congestion_control", cur, "bbr", "less sensitive to random loss than CUBIC"})
		}
	}
	return out
}

func readSysctl(key string) string {
	b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(key, ".", "/"))
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

func newTuneCmd() *cobra.Command {
	var rate, rtt float64
	var bbr, apply bool
	cmd := &cobra.Command{
		Use:   "tune",
		Short: "Suggest (or apply) TCP buffer sysctls so one stream can fill fast, long paths",
		Long: `Compare this host's TCP and socket buffer limits with what a single TCP
stream needs to reach --rate Mbit/s at --rtt ms (twice the bandwidth-delay
product), and print the changes. With --apply (root) they are written to
` + tuneFile + ` and loaded; delete that file and run 'sysctl --system'
to undo.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if rate <= 0 {
				rate = fastestLink()
			}
			adv := tuneAdvice(rate, rtt, bbr, readSysctl)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Target: one TCP stream at %.0f Mbit/s over %.0f ms RTT\n", rate, rtt)
			if len(adv) == 0 {
				fmt.Fprintln(out, "Nothing to change: this host's limits are already sufficient.")
				return nil
			}
			for _, a := range adv {
				fmt.Fprintf(out, "  %-32s %-22s → %-22s %s\n", a.key, orDash(a.current), a.want, a.why)
			}
			if !apply {
				fmt.Fprintf(out, "\nApply with: sudo pathgauge tune --rate %.0f --rtt %.0f%s --apply\n", rate, rtt, map[bool]string{true: " --bbr", false: ""}[bbr])
				return nil
			}
			return applyTune(out, adv, rate, rtt)
		},
	}
	cmd.Flags().Float64Var(&rate, "rate", 0, "target rate in Mbit/s (default: fastest interface speed, or 1000)")
	cmd.Flags().Float64Var(&rtt, "rtt", 100, "longest round-trip time to support, in ms")
	cmd.Flags().BoolVar(&bbr, "bbr", false, "also switch to BBR congestion control with the fq qdisc")
	cmd.Flags().BoolVar(&apply, "apply", false, "write "+tuneFile+" and load it (root)")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fastestLink() float64 {
	best := 0
	for _, ifc := range hostinfo.Collect(hostinfo.SystemSources()).Interfaces {
		if !ifc.Virtual && ifc.OperState == "up" && ifc.SpeedMbps > best {
			best = ifc.SpeedMbps
		}
	}
	if best == 0 {
		return 1000
	}
	return float64(best)
}

func applyTune(out io.Writer, adv []sysctlAdvice, rate, rtt float64) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--apply needs root")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by 'pathgauge tune' for %.0f Mbit/s at %.0f ms RTT.\n# Delete this file and run 'sysctl --system' to undo.\n", rate, rtt)
	for _, a := range adv {
		fmt.Fprintf(&b, "%s = %s\n", a.key, a.want)
	}
	if err := os.WriteFile(tuneFile, []byte(b.String()), 0o644); err != nil { //nolint:gosec // sysctl.d files are world-readable by convention
		return err
	}
	if o, err := exec.Command("sysctl", "-p", tuneFile).CombinedOutput(); err != nil {
		return fmt.Errorf("sysctl -p %s: %w: %s", tuneFile, err, o)
	}
	fmt.Fprintf(out, "\nWritten to %s and applied.\n", tuneFile)
	return nil
}
