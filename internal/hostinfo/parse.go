package hostinfo

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// KernelVersion is a parsed major.minor kernel release.
type KernelVersion struct {
	Major, Minor int
}

// ParseKernelRelease parses uname -r output such as "5.14.0-503.el9.x86_64".
func ParseKernelRelease(release string) (KernelVersion, error) {
	fields := strings.SplitN(release, ".", 3)
	if len(fields) < 2 {
		return KernelVersion{}, fmt.Errorf("unrecognised kernel release %q", release)
	}
	major, err := strconv.Atoi(fields[0])
	if err != nil {
		return KernelVersion{}, fmt.Errorf("unrecognised kernel release %q", release)
	}
	minor, err := strconv.Atoi(leadingDigits(fields[1]))
	if err != nil {
		return KernelVersion{}, fmt.Errorf("unrecognised kernel release %q", release)
	}
	return KernelVersion{Major: major, Minor: minor}, nil
}

// AtLeast reports whether k >= major.minor.
func (k KernelVersion) AtLeast(major, minor int) bool {
	return k.Major > major || (k.Major == major && k.Minor >= minor)
}

func (k KernelVersion) String() string { return fmt.Sprintf("%d.%d", k.Major, k.Minor) }

func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}

// Linux capability bit numbers (include/uapi/linux/capability.h).
const (
	CapNetBindService = 10
	CapNetAdmin       = 12
	CapNetRaw         = 13
)

// ParseCapEff extracts the effective capability mask from /proc/self/status.
func ParseCapEff(status string) (uint64, error) {
	for line := range strings.Lines(status) {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		}
	}
	return 0, fmt.Errorf("CapEff not found")
}

// HasCap reports whether capability bit c is set in mask.
func HasCap(mask uint64, c int) bool { return mask&(1<<uint(c)) != 0 }

// ParseGroupRange parses /proc/sys/net/ipv4/ping_group_range ("lo hi").
func ParseGroupRange(s string) (lo, hi int64, err error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unrecognised range %q", s)
	}
	if lo, err = strconv.ParseInt(f[0], 10, 64); err != nil {
		return 0, 0, err
	}
	if hi, err = strconv.ParseInt(f[1], 10, 64); err != nil {
		return 0, 0, err
	}
	return lo, hi, nil
}

// ParseTriple parses sysctls such as net.ipv4.tcp_rmem ("min default max").
func ParseTriple(s string) ([3]int64, error) {
	var out [3]int64
	f := strings.Fields(s)
	if len(f) != 3 {
		return out, fmt.Errorf("want 3 values, got %q", s)
	}
	for i, v := range f {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return out, err
		}
		out[i] = n
	}
	return out, nil
}

// ModuleSet records kernel modules by normalised name (dashes become
// underscores, as the kernel does).
type ModuleSet map[string]bool

func normaliseModule(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// Has reports whether the set contains the module.
func (m ModuleSet) Has(name string) bool { return m[normaliseModule(name)] }

// AddModulePaths adds modules from modules.dep or modules.builtin content,
// where each line starts with a path like kernel/net/sched/sch_cake.ko.zst.
func (m ModuleSet) AddModulePaths(content string) {
	for line := range strings.Lines(content) {
		p, _, _ := strings.Cut(line, ":")
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		base := path.Base(p)
		if i := strings.Index(base, ".ko"); i > 0 {
			m[normaliseModule(base[:i])] = true
		}
	}
}

// AddProcModules adds modules listed in /proc/modules.
func (m ModuleSet) AddProcModules(content string) {
	for line := range strings.Lines(content) {
		if f := strings.Fields(line); len(f) > 0 {
			m[normaliseModule(f[0])] = true
		}
	}
}

// ParseOSRelease parses /etc/os-release key=value lines.
func ParseOSRelease(content string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `'"`)
		}
		out[k] = v
	}
	return out
}

// ParseUevent extracts DRIVER= from a sysfs uevent file.
func ParseUevent(content, key string) string {
	for line := range strings.Lines(content) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v
		}
	}
	return ""
}

// MaxRateForWindow returns the highest single-stream rate in bits/s that a
// window of the given size can sustain at the given RTT. It is an upper
// bound: the kernel reserves part of the buffer for overhead.
func MaxRateForWindow(windowBytes int64, rttSeconds float64) float64 {
	if rttSeconds <= 0 {
		return 0
	}
	return float64(windowBytes) * 8 / rttSeconds
}

// FormatRate renders bits/s with an SI prefix, for example "2.5 Gbit/s".
func FormatRate(bps float64) string {
	switch {
	case bps >= 1e9:
		return trimFloat(bps/1e9) + " Gbit/s"
	case bps >= 1e6:
		return trimFloat(bps/1e6) + " Mbit/s"
	case bps >= 1e3:
		return trimFloat(bps/1e3) + " kbit/s"
	default:
		return trimFloat(bps) + " bit/s"
	}
}

// FormatBytes renders a byte count in binary units, for example "6 MiB".
func FormatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return trimFloat(float64(n)/(1<<30)) + " GiB"
	case n >= 1<<20:
		return trimFloat(float64(n)/(1<<20)) + " MiB"
	case n >= 1<<10:
		return trimFloat(float64(n)/(1<<10)) + " KiB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}
