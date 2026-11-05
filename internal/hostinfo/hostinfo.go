// Package hostinfo inspects the local host: kernel features, privileges,
// interfaces and tuning that affect network test results. It backs
// `pathgauge doctor` and, later, the per-run host diagnostics snapshot.
package hostinfo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Status grades one check.
type Status string

// Check statuses.
const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusMissing Status = "missing"
	StatusInfo    Status = "info"
)

// Check is one readiness item.
type Check struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"`
	// Impact explains what is degraded when the check is not ok.
	Impact string `json:"impact,omitempty"`
	// Fix suggests how to resolve it.
	Fix string `json:"fix,omitempty"`
}

// Interface summarises one network interface.
type Interface struct {
	Name      string `json:"name"`
	OperState string `json:"oper_state"`
	MTU       int    `json:"mtu"`
	// SpeedMbps is the negotiated link speed, or 0 when unknown.
	SpeedMbps int    `json:"speed_mbps"`
	Driver    string `json:"driver,omitempty"`
	Wireless  bool   `json:"wireless"`
	Virtual   bool   `json:"virtual"`
}

// Report is the full readiness report.
type Report struct {
	Hostname       string      `json:"hostname"`
	OS             string      `json:"os"`
	OSID           string      `json:"os_id"`
	Kernel         string      `json:"kernel"`
	Arch           string      `json:"arch"`
	CPUs           int         `json:"cpus"`
	Virtualization string      `json:"virtualization,omitempty"`
	Interfaces     []Interface `json:"interfaces"`
	Checks         []Check     `json:"checks"`
}

// Counts returns how many checks have each status.
func (r Report) Counts() map[Status]int {
	out := map[Status]int{}
	for _, c := range r.Checks {
		out[c.Status]++
	}
	return out
}

// ClockState is the kernel's view of time synchronisation.
type ClockState struct {
	Synced bool
	// EstErrorMicros and MaxErrorMicros come from adjtimex.
	EstErrorMicros int64
	MaxErrorMicros int64
}

// Sources lets tests replace every input. Use SystemSources in production.
type Sources struct {
	// Root is the filesystem holding proc/, sys/, etc/ and lib/modules/.
	Root     fs.FS
	Release  string // uname -r
	Hostname string
	Arch     string
	CPUs     int
	UID, GID int
	Groups   []int
	// LookPath finds an executable by name.
	LookPath func(name string) (string, error)
	// Clock reads time-synchronisation state.
	Clock func() (ClockState, error)
	// Virt names the virtualisation technology, or "" when none or unknown.
	Virt func() string
}

// SystemSources reads the running system.
func SystemSources() Sources {
	var uts unix.Utsname
	release := ""
	if err := unix.Uname(&uts); err == nil {
		release = unix.ByteSliceToString(uts.Release[:])
	}
	host, _ := os.Hostname()
	groups, _ := os.Getgroups()
	return Sources{
		Root:     os.DirFS("/"),
		Release:  release,
		Hostname: host,
		Arch:     runtime.GOARCH,
		CPUs:     runtime.NumCPU(),
		UID:      os.Geteuid(),
		GID:      os.Getegid(),
		Groups:   groups,
		LookPath: lookPathWithSbin,
		Clock:    adjtimexClock,
		Virt:     detectVirt,
	}
}

// lookPathWithSbin also searches the sbin directories, which are often
// missing from unprivileged users' PATH but hold tc and ip.
func lookPathWithSbin(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin"} {
		p := dir + "/" + name
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// timeError is the adjtimex return value for an unsynchronised clock.
const timeError = 5

func adjtimexClock() (ClockState, error) {
	var tx unix.Timex // Modes == 0: read-only, needs no privilege
	state, err := unix.Adjtimex(&tx)
	if err != nil {
		return ClockState{}, err
	}
	return ClockState{
		Synced:         state != timeError && tx.Status&unix.STA_UNSYNC == 0,
		EstErrorMicros: tx.Esterror, // int64 on amd64 and arm64, the supported targets
		MaxErrorMicros: tx.Maxerror,
	}, nil
}

func detectVirt() string {
	path, err := lookPathWithSbin("systemd-detect-virt")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path).Output() // exits 1 when "none"
	v := strings.TrimSpace(string(out))
	if v == "none" {
		return ""
	}
	return v
}

// Collect builds the readiness report.
func Collect(src Sources) Report {
	osr := ParseOSRelease(readOr(src.Root, "etc/os-release", ""))
	r := Report{
		Hostname: src.Hostname,
		OS:       osr["PRETTY_NAME"],
		OSID:     osr["ID"],
		Kernel:   src.Release,
		Arch:     src.Arch,
		CPUs:     src.CPUs,
	}
	if src.Virt != nil {
		r.Virtualization = src.Virt()
	}
	r.Interfaces = collectInterfaces(src.Root)

	c := &collector{src: src, osID: osr["ID"], osLike: osr["ID_LIKE"], osVersion: osr["VERSION_ID"]}
	c.modules = c.loadModules()
	c.privileges()
	c.kernel()
	c.congestionControl()
	c.trafficControl()
	c.buffers()
	c.clock()
	c.host(r)
	r.Checks = c.checks
	return r
}

type collector struct {
	src          Sources
	osID, osLike string
	osVersion    string
	modules      ModuleSet
	modulesKnown bool
	capEff       uint64
	capKnown     bool
	checks       []Check
}

func (c *collector) add(ch Check) { c.checks = append(c.checks, ch) }

func (c *collector) read(name string) (string, error) {
	b, err := fs.ReadFile(c.src.Root, name)
	return strings.TrimSpace(string(b)), err
}

func readOr(root fs.FS, name, fallback string) string {
	b, err := fs.ReadFile(root, name)
	if err != nil {
		return fallback
	}
	return string(b)
}

func (c *collector) loadModules() ModuleSet {
	m := ModuleSet{}
	if s, err := c.read("proc/modules"); err == nil {
		m.AddProcModules(s)
	}
	for _, base := range []string{"lib/modules/", "usr/lib/modules/"} {
		dir := base + c.src.Release + "/"
		dep, errDep := c.read(dir + "modules.dep")
		builtin, errBuiltin := c.read(dir + "modules.builtin")
		if errDep == nil || errBuiltin == nil {
			m.AddModulePaths(dep)
			m.AddModulePaths(builtin)
			c.modulesKnown = true
			break
		}
	}
	return m
}

func (c *collector) isRHELLike() bool {
	return slices.Contains([]string{"rhel", "rocky", "almalinux", "centos", "fedora"}, c.osID) ||
		strings.Contains(c.osLike, "rhel") || strings.Contains(c.osLike, "fedora")
}

// isEL8 reports an Enterprise Linux 8 family host (Rocky, RHEL, Alma 8).
func (c *collector) isEL8() bool {
	return c.isRHELLike() && c.osID != "fedora" && (c.osVersion == "8" || strings.HasPrefix(c.osVersion, "8."))
}

func (c *collector) isDebianLike() bool {
	return c.osID == "ubuntu" || c.osID == "debian" || strings.Contains(c.osLike, "debian")
}

// extraModulesFix names the distro package that holds less common modules
// such as sch_cake, sch_netem and ifb.
func (c *collector) extraModulesFix() string {
	switch {
	case c.isEL8():
		return "sudo dnf install kernel-modules-extra   (then reboot if the running kernel was updated)"
	case c.isRHELLike():
		// EL9 and later ship these in kernel-modules-core, others in -extra.
		return "sudo dnf install kernel-modules-core kernel-modules-extra   (then reboot if the running kernel was updated)"
	case c.osID == "ubuntu":
		return "sudo apt install linux-modules-extra-" + c.src.Release
	default:
		return "install your distribution's extra kernel modules package"
	}
}

func (c *collector) privileges() {
	const section = "Privileges"
	status, err := c.read("proc/self/status")
	if err == nil {
		c.capEff, err = ParseCapEff(status)
	}
	c.capKnown = err == nil
	root := c.src.UID == 0

	capCheck := func(name string, bit int, impact string) {
		switch {
		case !c.capKnown:
			c.add(Check{Section: section, Name: name, Status: StatusWarn, Detail: "could not read process capabilities", Impact: impact})
		case HasCap(c.capEff, bit):
			c.add(Check{Section: section, Name: name, Status: StatusOK, Detail: "held by this process"})
		default:
			c.add(Check{Section: section, Name: name, Status: StatusWarn,
				Detail: "not held by this process (the systemd services grant it)",
				Impact: impact,
				Fix:    "run tests through the service, or with sudo"})
		}
	}
	if root && c.capKnown && HasCap(c.capEff, CapNetRaw) && HasCap(c.capEff, CapNetAdmin) {
		c.add(Check{Section: section, Name: "Running as root", Status: StatusOK, Detail: "all network capabilities available"})
	} else {
		capCheck("CAP_NET_RAW", CapNetRaw, "raw ICMP probes, TCP-SYN traceroute and packet capture unavailable")
		capCheck("CAP_NET_ADMIN", CapNetAdmin, "traffic shaping and forced large socket buffers unavailable")
	}

	if s, err := c.read("proc/sys/net/ipv4/ping_group_range"); err == nil {
		lo, hi, perr := ParseGroupRange(s)
		gids := append([]int{c.src.GID}, c.src.Groups...)
		allowed := perr == nil && slices.ContainsFunc(gids, func(g int) bool { return int64(g) >= lo && int64(g) <= hi })
		if allowed {
			c.add(Check{Section: section, Name: "Unprivileged ICMP sockets", Status: StatusOK, Detail: "ping_group_range includes this user"})
		} else {
			c.add(Check{Section: section, Name: "Unprivileged ICMP sockets", Status: StatusInfo,
				Detail: fmt.Sprintf("ping_group_range %q excludes this user; ICMP needs CAP_NET_RAW", s)})
		}
	}
}

func (c *collector) kernel() {
	const section = "Kernel"
	kv, err := ParseKernelRelease(c.src.Release)
	if err != nil {
		c.add(Check{Section: section, Name: "Kernel version", Status: StatusWarn, Detail: err.Error()})
		return
	}
	c.add(Check{Section: section, Name: "Kernel version", Status: StatusInfo, Detail: c.src.Release})

	feature := func(name string, major, minor int, impact string) {
		if kv.AtLeast(major, minor) {
			c.add(Check{Section: section, Name: name, Status: StatusOK, Detail: fmt.Sprintf("kernel %s >= %d.%d", kv, major, minor)})
			return
		}
		ch := Check{Section: section, Name: name, Status: StatusMissing,
			Detail: fmt.Sprintf("kernel %s < %d.%d", kv, major, minor), Impact: impact}
		if c.isRHELLike() {
			// Enterprise kernels backport features without bumping the version.
			ch.Status = StatusWarn
			ch.Detail += " (may be backported; confirmed at test time)"
		}
		c.add(ch)
	}
	feature("TCP_INFO limit statistics", 4, 10, "cannot tell receiver-window, sender-buffer and network limits apart")
	feature("UDP segmentation offload (GSO)", 4, 18, "UDP tests above ~2 Gbit/s need much more CPU")
	feature("UDP receive offload (GRO)", 5, 0, "UDP receive above ~2 Gbit/s needs much more CPU")

	if s, err := c.read("proc/net/if_inet6"); err == nil && s != "" {
		c.add(Check{Section: section, Name: "IPv6", Status: StatusOK, Detail: "enabled"})
	} else {
		c.add(Check{Section: section, Name: "IPv6", Status: StatusInfo, Detail: "disabled or no addresses; IPv6 tests unavailable"})
	}
}

func (c *collector) congestionControl() {
	const section = "Congestion control"
	avail, _ := c.read("proc/sys/net/ipv4/tcp_available_congestion_control")
	def, _ := c.read("proc/sys/net/ipv4/tcp_congestion_control")
	algs := strings.Fields(avail)

	loadable := []string{}
	for _, a := range []string{"bbr", "cubic", "reno", "htcp", "vegas", "westwood"} {
		if !slices.Contains(algs, a) && c.modules.Has("tcp_"+a) {
			loadable = append(loadable, a)
		}
	}
	detail := "available: " + strings.Join(algs, ", ")
	if len(loadable) > 0 {
		detail += "; loadable: " + strings.Join(loadable, ", ")
	}
	if def != "" {
		detail += "; default: " + def
	}
	c.add(Check{Section: section, Name: "Algorithms", Status: StatusInfo, Detail: detail})

	switch {
	case slices.Contains(algs, "bbr"):
		c.add(Check{Section: section, Name: "BBR", Status: StatusOK, Detail: "available"})
	case slices.Contains(loadable, "bbr"):
		c.add(Check{Section: section, Name: "BBR", Status: StatusOK,
			Detail: "module present; the kernel loads it when a test selects it (needs CAP_NET_ADMIN)"})
	default:
		c.add(Check{Section: section, Name: "BBR", Status: StatusMissing, Detail: "not available",
			Impact: "congestion-control comparison limited to the algorithms above"})
	}
}

func (c *collector) trafficControl() {
	const section = "Traffic control"
	if p, err := c.src.LookPath("tc"); err == nil {
		c.add(Check{Section: section, Name: "tc (iproute2)", Status: StatusOK, Detail: p})
	} else {
		fix := "install iproute2"
		if c.isRHELLike() {
			fix = "sudo dnf install iproute-tc"
		} else if c.isDebianLike() {
			fix = "sudo apt install iproute2"
		}
		c.add(Check{Section: section, Name: "tc (iproute2)", Status: StatusMissing, Detail: "not found",
			Impact: "traffic shaping and the SQM tuner unavailable", Fix: fix})
	}

	if def, err := c.read("proc/sys/net/core/default_qdisc"); err == nil {
		c.add(Check{Section: section, Name: "Default qdisc", Status: StatusInfo, Detail: def})
	}

	if !c.modulesKnown {
		c.add(Check{Section: section, Name: "Queueing modules", Status: StatusWarn,
			Detail: "module index not found for kernel " + c.src.Release + "; availability is checked at test time"})
		return
	}
	mods := []struct{ module, label, impact string }{
		{"sch_fq", "fq", "paced TCP sweeps less precise"},
		{"sch_fq_codel", "fq_codel", "SQM what-if tests limited"},
		{"sch_cake", "CAKE", "SQM tuner cannot test CAKE"},
		{"sch_netem", "netem", "delay/loss emulation unavailable"},
		{"sch_tbf", "tbf", "simple rate limiting unavailable"},
		{"sch_htb", "htb", "hierarchical shaping unavailable"},
		{"ifb", "IFB (ingress shaping)", "only outbound traffic can be shaped"},
	}
	for _, m := range mods {
		if c.modules.Has(m.module) {
			c.add(Check{Section: section, Name: m.label, Status: StatusOK, Detail: "available"})
			continue
		}
		if m.module == "sch_cake" && c.isEL8() {
			c.add(Check{Section: section, Name: m.label, Status: StatusMissing,
				Detail: "not shipped by Enterprise Linux 8 kernels",
				Impact: "the SQM tuner uses fq_codel with htb instead of CAKE on this host"})
			continue
		}
		c.add(Check{Section: section, Name: m.label, Status: StatusMissing, Detail: "module " + m.module + " not found",
			Impact: m.impact, Fix: c.extraModulesFix()})
	}
}

func (c *collector) buffers() {
	const section = "TCP/UDP buffers"
	for _, sysctl := range []struct{ file, label string }{
		{"proc/sys/net/ipv4/tcp_rmem", "tcp_rmem (receive)"},
		{"proc/sys/net/ipv4/tcp_wmem", "tcp_wmem (send)"},
	} {
		s, err := c.read(sysctl.file)
		if err != nil {
			continue
		}
		v, err := ParseTriple(s)
		if err != nil {
			continue
		}
		maxBytes := v[2]
		at20 := MaxRateForWindow(maxBytes, 0.020)
		at100 := MaxRateForWindow(maxBytes, 0.100)
		ch := Check{Section: section, Name: sysctl.label, Status: StatusOK,
			Detail: fmt.Sprintf("max %s: one stream can reach at most ~%s at 20 ms RTT, ~%s at 100 ms",
				FormatBytes(maxBytes), FormatRate(at20), FormatRate(at100))}
		if at20 < 1e9 {
			ch.Status = StatusWarn
			ch.Impact = "single-stream results will be limited by this host, not the network"
			ch.Fix = "raise the third value of net.ipv4." + strings.Fields(sysctl.label)[0] + " (pathgauge tune, planned, will suggest values)"
		}
		c.add(ch)
	}

	rmemMax, err := c.read("proc/sys/net/core/rmem_max")
	if err != nil {
		return
	}
	n, err := strconv.ParseInt(rmemMax, 10, 64)
	if err != nil {
		return
	}
	ch := Check{Section: section, Name: "net.core.rmem_max", Status: StatusOK, Detail: FormatBytes(n)}
	if n < 4<<20 {
		if c.capKnown && HasCap(c.capEff, CapNetAdmin) {
			ch.Status = StatusInfo
			ch.Detail += " (low, but bypassed with SO_RCVBUFFORCE because CAP_NET_ADMIN is held)"
		} else {
			ch.Status = StatusWarn
			ch.Detail += " is low for high-rate UDP"
			ch.Impact = "UDP tests may drop packets in this host's socket buffer and report false loss"
			ch.Fix = "run via the service (CAP_NET_ADMIN lifts the limit) or: sudo sysctl -w net.core.rmem_max=16777216"
		}
	}
	c.add(ch)
}

func (c *collector) clock() {
	const section = "Time"
	if c.src.Clock == nil {
		return
	}
	st, err := c.src.Clock()
	switch {
	case err != nil:
		c.add(Check{Section: section, Name: "Clock synchronisation", Status: StatusWarn, Detail: "adjtimex failed: " + err.Error()})
	case st.Synced:
		c.add(Check{Section: section, Name: "Clock synchronisation", Status: StatusOK,
			Detail: fmt.Sprintf("synchronised (estimated error %s, maximum %s)",
				formatMicros(st.EstErrorMicros), formatMicros(st.MaxErrorMicros))})
	default:
		c.add(Check{Section: section, Name: "Clock synchronisation", Status: StatusWarn, Detail: "not synchronised",
			Impact: "one-way delay results will be hidden; round-trip results are unaffected",
			Fix:    "enable chrony (Rocky: sudo systemctl enable --now chronyd; Ubuntu: sudo apt install chrony)"})
	}
}

func formatMicros(us int64) string {
	if us >= 1000 {
		return strconv.FormatFloat(float64(us)/1000, 'f', 1, 64) + " ms"
	}
	return strconv.FormatInt(us, 10) + " µs"
}

func (c *collector) host(r Report) {
	const section = "Host"
	c.add(Check{Section: section, Name: "CPUs", Status: StatusInfo, Detail: strconv.Itoa(r.CPUs)})
	if r.Virtualization != "" {
		c.add(Check{Section: section, Name: "Virtualisation", Status: StatusInfo,
			Detail: r.Virtualization + ": check CPU steal time during tests; container networking can skew results"})
	}
	for _, ifc := range r.Interfaces {
		if ifc.Wireless && ifc.OperState == "up" {
			c.add(Check{Section: section, Name: "Wi-Fi interface " + ifc.Name, Status: StatusInfo,
				Detail: "results include the wireless hop; Wi-Fi signal details arrive in a later release"})
		}
	}
}

// hiddenInterfacePrefixes are container and VM plumbing that would clutter
// the report.
var hiddenInterfacePrefixes = []string{"veth", "docker", "br-", "virbr", "vnet", "cni", "flannel", "cali", "lxc"}

func collectInterfaces(root fs.FS) []Interface {
	entries, err := fs.ReadDir(root, "sys/class/net")
	if err != nil {
		return nil
	}
	var out []Interface
	for _, e := range entries {
		name := e.Name()
		if name == "lo" || slices.ContainsFunc(hiddenInterfacePrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		dir := "sys/class/net/" + name + "/"
		ifc := Interface{Name: name, OperState: strings.TrimSpace(readOr(root, dir+"operstate", "unknown"))}
		ifc.MTU, _ = strconv.Atoi(strings.TrimSpace(readOr(root, dir+"mtu", "0")))
		if speed, err := strconv.Atoi(strings.TrimSpace(readOr(root, dir+"speed", ""))); err == nil && speed > 0 {
			ifc.SpeedMbps = speed
		}
		uevent, err := fs.ReadFile(root, dir+"device/uevent")
		switch {
		case err == nil:
			ifc.Driver = ParseUevent(string(uevent), "DRIVER")
		case errors.Is(err, fs.ErrNotExist):
			ifc.Virtual = true
		}
		if _, err := fs.Stat(root, dir+"wireless"); err == nil {
			ifc.Wireless = true
		} else if _, err := fs.Stat(root, dir+"phy80211"); err == nil {
			ifc.Wireless = true
		}
		out = append(out, ifc)
	}
	return out
}
