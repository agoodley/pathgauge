package hostinfo

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

var statusTag = map[Status]string{
	StatusOK:      "[ ok ]",
	StatusWarn:    "[warn]",
	StatusMissing: "[miss]",
	StatusInfo:    "[info]",
}

// WriteText renders the report for a terminal.
func WriteText(w io.Writer, r Report) error {
	var b strings.Builder
	fmt.Fprintln(&b, "PathGauge doctor: host readiness report")
	osName := r.OS
	if osName == "" {
		osName = "unknown OS"
	}
	fmt.Fprintf(&b, "Host: %s (%s, kernel %s, %s, %d CPUs)\n", r.Hostname, osName, r.Kernel, r.Arch, r.CPUs)

	if len(r.Interfaces) > 0 {
		fmt.Fprintln(&b, "\nInterfaces")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, i := range r.Interfaces {
			speed := "-"
			if i.SpeedMbps > 0 {
				speed = fmt.Sprintf("%d Mb/s", i.SpeedMbps)
			}
			var notes []string
			if i.Driver != "" {
				notes = append(notes, "driver "+i.Driver)
			}
			if i.Wireless {
				notes = append(notes, "wireless")
			}
			if i.Virtual {
				notes = append(notes, "virtual")
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\tmtu %d\t%s\n", i.Name, i.OperState, speed, i.MTU, strings.Join(notes, ", "))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	section := ""
	for _, c := range r.Checks {
		if c.Section != section {
			section = c.Section
			fmt.Fprintf(&b, "\n%s\n", section)
		}
		fmt.Fprintf(&b, "  %s %s: %s\n", statusTag[c.Status], c.Name, c.Detail)
		if c.Impact != "" {
			fmt.Fprintf(&b, "         impact: %s\n", c.Impact)
		}
		if c.Fix != "" {
			fmt.Fprintf(&b, "         fix:    %s\n", c.Fix)
		}
	}

	n := r.Counts()
	fmt.Fprintf(&b, "\nSummary: %d ok, %d warnings, %d missing\n", n[StatusOK], n[StatusWarn], n[StatusMissing])
	_, err := io.WriteString(w, b.String())
	return err
}
