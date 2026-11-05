package hostinfo

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseKernelRelease(t *testing.T) {
	cases := map[string]KernelVersion{
		"7.0.0-42-generic":          {7, 0},
		"4.18.0-553.el8_10.x86_64":  {4, 18},
		"5.14.0-503.el9.x86_64":     {5, 14},
		"6.12.0-55.el10_0.aarch64":  {6, 12},
		"6.8.0-rc3":                 {6, 8},
		"5.15.167.4-microsoft-wsl2": {5, 15},
	}
	for in, want := range cases {
		got, err := ParseKernelRelease(in)
		if err != nil || got != want {
			t.Errorf("ParseKernelRelease(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "seven", "6"} {
		if _, err := ParseKernelRelease(bad); err == nil {
			t.Errorf("ParseKernelRelease(%q) should fail", bad)
		}
	}
	k := KernelVersion{4, 18}
	if !k.AtLeast(4, 10) || !k.AtLeast(4, 18) || k.AtLeast(5, 0) || !k.AtLeast(3, 99) {
		t.Error("AtLeast wrong")
	}
}

func TestParseCapEff(t *testing.T) {
	status := "Name:\tpathgauge\nCapInh:\t0000000000000000\nCapEff:\t0000000000003000\n"
	mask, err := ParseCapEff(status)
	if err != nil {
		t.Fatal(err)
	}
	if !HasCap(mask, CapNetAdmin) || !HasCap(mask, CapNetRaw) || HasCap(mask, CapNetBindService) {
		t.Errorf("mask %x decoded wrongly", mask)
	}
	if _, err := ParseCapEff("Name: x\n"); err == nil {
		t.Error("missing CapEff should fail")
	}
}

func TestModuleSet(t *testing.T) {
	m := ModuleSet{}
	m.AddModulePaths("kernel/net/sched/sch_cake.ko.zst: kernel/lib/x.ko.zst\nkernel/drivers/net/ifb.ko.xz:\n")
	m.AddModulePaths("kernel/net/sched/sch_fq_codel.ko\n")
	m.AddProcModules("tcp_bbr 20480 3 - Live 0x0000000000000000\n")
	for _, name := range []string{"sch_cake", "ifb", "sch_fq_codel", "tcp_bbr", "sch-cake"} {
		if !m.Has(name) {
			t.Errorf("missing %s", name)
		}
	}
	if m.Has("sch_netem") {
		t.Error("unexpected sch_netem")
	}
}

func TestParseOSRelease(t *testing.T) {
	got := ParseOSRelease("NAME=\"Rocky Linux\"\nID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n# comment\nPRETTY_NAME='Rocky Linux 9.6 (Blue Onyx)'\n")
	if got["ID"] != "rocky" || got["ID_LIKE"] != "rhel centos fedora" || got["PRETTY_NAME"] != "Rocky Linux 9.6 (Blue Onyx)" {
		t.Errorf("got %v", got)
	}
}

func TestFormatting(t *testing.T) {
	if got := FormatRate(MaxRateForWindow(6291456, 0.020)); got != "2.5 Gbit/s" {
		t.Errorf("FormatRate = %q", got)
	}
	if got := FormatRate(940e6); got != "940 Mbit/s" {
		t.Errorf("FormatRate = %q", got)
	}
	if got := FormatBytes(6291456); got != "6 MiB" {
		t.Errorf("FormatBytes = %q", got)
	}
	if got := FormatBytes(212992); got != "208 KiB" {
		t.Errorf("FormatBytes = %q", got)
	}
}

// fakeHost returns a minimal Rocky 8-like host with an unprivileged process.
func fakeHost() Sources {
	release := "4.18.0-553.el8_10.x86_64"
	root := fstest.MapFS{
		"etc/os-release":                                     {Data: []byte("ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"8.10\"\nPRETTY_NAME=\"Rocky Linux 8.10\"\n")},
		"proc/self/status":                                   {Data: []byte("CapEff:\t0000000000000000\n")},
		"proc/sys/net/ipv4/ping_group_range":                 {Data: []byte("1\t0\n")},
		"proc/net/if_inet6":                                  {Data: []byte("00000000000000000000000000000001 01 80 10 80       lo\n")},
		"proc/sys/net/ipv4/tcp_available_congestion_control": {Data: []byte("reno cubic\n")},
		"proc/sys/net/ipv4/tcp_congestion_control":           {Data: []byte("cubic\n")},
		"proc/sys/net/core/default_qdisc":                    {Data: []byte("fq_codel\n")},
		"proc/sys/net/ipv4/tcp_rmem":                         {Data: []byte("4096\t131072\t6291456\n")},
		"proc/sys/net/ipv4/tcp_wmem":                         {Data: []byte("4096\t16384\t4194304\n")},
		"proc/sys/net/core/rmem_max":                         {Data: []byte("212992\n")},
		"proc/modules":                                       {Data: []byte("sch_fq_codel 20480 2 - Live 0x0\n")},
		"lib/modules/" + release + "/modules.dep": {Data: []byte(
			"kernel/net/ipv4/tcp_bbr.ko.xz:\nkernel/net/sched/sch_fq.ko.xz:\nkernel/net/sched/sch_tbf.ko.xz:\nkernel/net/sched/sch_htb.ko.xz:\n")},
		"lib/modules/" + release + "/modules.builtin": {Data: []byte("")},
		"sys/class/net/eth0/operstate":                {Data: []byte("up\n")},
		"sys/class/net/eth0/mtu":                      {Data: []byte("1500\n")},
		"sys/class/net/eth0/speed":                    {Data: []byte("1000\n")},
		"sys/class/net/eth0/device/uevent":            {Data: []byte("DRIVER=igb\nPCI_CLASS=20000\n")},
		"sys/class/net/wlan0/operstate":               {Data: []byte("up\n")},
		"sys/class/net/wlan0/mtu":                     {Data: []byte("1500\n")},
		"sys/class/net/wlan0/speed":                   {Data: []byte("-1\n")},
		"sys/class/net/wlan0/device/uevent":           {Data: []byte("DRIVER=iwlwifi\n")},
		"sys/class/net/wlan0/wireless/.keep":          {Data: []byte{}},
		"sys/class/net/vethabc/operstate":             {Data: []byte("up\n")},
		"sys/class/net/lo/operstate":                  {Data: []byte("unknown\n")},
		"sys/class/net/br0/operstate":                 {Data: []byte("up\n")},
		"sys/class/net/br0/mtu":                       {Data: []byte("9000\n")},
	}
	return Sources{
		Root: root, Release: release, Hostname: "box-b", Arch: "amd64", CPUs: 4,
		UID: 990, GID: 990,
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		Clock:    func() (ClockState, error) { return ClockState{Synced: false}, nil },
		Virt:     func() string { return "kvm" },
	}
}

func find(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in %+v", name, r.Checks)
	return Check{}
}

func TestCollectFakeRocky8(t *testing.T) {
	r := Collect(fakeHost())

	if r.OS != "Rocky Linux 8.10" || r.OSID != "rocky" || r.Virtualization != "kvm" {
		t.Errorf("host fields wrong: %+v", r)
	}

	if len(r.Interfaces) != 3 {
		t.Fatalf("want eth0, wlan0, br0 (lo and veth hidden), got %+v", r.Interfaces)
	}
	byName := map[string]Interface{}
	for _, i := range r.Interfaces {
		byName[i.Name] = i
	}
	if e := byName["eth0"]; e.SpeedMbps != 1000 || e.Driver != "igb" || e.MTU != 1500 || e.Virtual {
		t.Errorf("eth0 = %+v", e)
	}
	if w := byName["wlan0"]; !w.Wireless || w.SpeedMbps != 0 {
		t.Errorf("wlan0 = %+v", w)
	}
	if b := byName["br0"]; !b.Virtual || b.MTU != 9000 {
		t.Errorf("br0 = %+v", b)
	}

	expect := map[string]Status{
		"CAP_NET_RAW":                    StatusWarn,
		"CAP_NET_ADMIN":                  StatusWarn,
		"Unprivileged ICMP sockets":      StatusInfo,
		"TCP_INFO limit statistics":      StatusOK,
		"UDP segmentation offload (GSO)": StatusOK,
		"UDP receive offload (GRO)":      StatusWarn, // RHEL-like: maybe backported
		"BBR":                            StatusOK,   // loadable module
		"tc (iproute2)":                  StatusMissing,
		"fq":                             StatusOK,
		"fq_codel":                       StatusOK, // loaded, from /proc/modules
		"CAKE":                           StatusMissing,
		"IFB (ingress shaping)":          StatusMissing,
		"tcp_rmem (receive)":             StatusOK,
		"tcp_wmem (send)":                StatusOK, // 4 MiB is ~1.7 Gbit/s at 20 ms
		"net.core.rmem_max":              StatusWarn,
		"Clock synchronisation":          StatusWarn,
		"Wi-Fi interface wlan0":          StatusInfo,
	}
	for name, want := range expect {
		if got := find(t, r, name); got.Status != want {
			t.Errorf("%s: status %s, want %s (%s)", name, got.Status, want, got.Detail)
		}
	}

	if cake := find(t, r, "CAKE"); cake.Fix != "" || !strings.Contains(cake.Detail, "Enterprise Linux 8") || !strings.Contains(cake.Impact, "fq_codel") {
		t.Errorf("EL8 has no CAKE to install; got %+v", cake)
	}
	if fix := find(t, r, "netem").Fix; !strings.Contains(fix, "kernel-modules-extra") {
		t.Errorf("netem fix should point at kernel-modules-extra, got %q", fix)
	}
	if fix := find(t, r, "tc (iproute2)").Fix; fix != "sudo dnf install iproute-tc" {
		t.Errorf("tc fix = %q", fix)
	}
	if !strings.Contains(find(t, r, "Algorithms").Detail, "loadable: bbr") {
		t.Error("BBR should be listed as loadable")
	}

	var buf bytes.Buffer
	if err := WriteText(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Host: box-b (Rocky Linux 8.10", "[miss] CAKE: not shipped by Enterprise Linux 8", "fix:    sudo dnf install kernel-modules-extra", "Summary:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
}

func TestCollectRootUbuntu(t *testing.T) {
	src := fakeHost()
	root := src.Root.(fstest.MapFS)
	root["etc/os-release"] = &fstest.MapFile{Data: []byte("ID=ubuntu\nID_LIKE=debian\nPRETTY_NAME=\"Ubuntu 26.04 LTS\"\n")}
	root["proc/self/status"] = &fstest.MapFile{Data: []byte("CapEff:\t000001ffffffffff\n")}
	root["proc/sys/net/ipv4/tcp_rmem"] = &fstest.MapFile{Data: []byte("4096 131072 1048576\n")}
	src.Release = "7.0.0-42-generic"
	root["lib/modules/7.0.0-42-generic/modules.dep"] = &fstest.MapFile{Data: []byte("kernel/net/sched/sch_cake.ko.zst:\n")}
	src.UID, src.GID = 0, 0
	src.LookPath = func(string) (string, error) { return "/usr/sbin/tc", nil }
	src.Clock = func() (ClockState, error) {
		return ClockState{Synced: true, EstErrorMicros: 120, MaxErrorMicros: 4500}, nil
	}

	r := Collect(src)
	if c := find(t, r, "Running as root"); c.Status != StatusOK {
		t.Errorf("root check = %+v", c)
	}
	if c := find(t, r, "UDP receive offload (GRO)"); c.Status != StatusOK {
		t.Errorf("GRO on 7.0 = %+v", c)
	}
	if c := find(t, r, "CAKE"); c.Status != StatusOK {
		t.Errorf("CAKE = %+v", c)
	}
	if c := find(t, r, "netem"); !strings.Contains(c.Fix, "linux-modules-extra-7.0.0-42-generic") {
		t.Errorf("netem fix = %q", c.Fix)
	}
	if c := find(t, r, "tcp_rmem (receive)"); c.Status != StatusWarn {
		t.Errorf("1 MiB rmem should warn: %+v", c)
	}
	if c := find(t, r, "net.core.rmem_max"); c.Status != StatusInfo {
		t.Errorf("rmem_max with CAP_NET_ADMIN should be info: %+v", c)
	}
	if c := find(t, r, "Clock synchronisation"); c.Status != StatusOK || !strings.Contains(c.Detail, "120 µs") || !strings.Contains(c.Detail, "4.5 ms") {
		t.Errorf("clock = %+v", c)
	}
}

func TestCollectMissingModuleIndex(t *testing.T) {
	src := fakeHost()
	src.Release = "6.1.0-unknown"
	src.Clock = func() (ClockState, error) { return ClockState{}, errors.New("EPERM") }
	r := Collect(src)
	if c := find(t, r, "Queueing modules"); c.Status != StatusWarn {
		t.Errorf("missing index should warn: %+v", c)
	}
	if c := find(t, r, "Clock synchronisation"); c.Status != StatusWarn || !strings.Contains(c.Detail, "EPERM") {
		t.Errorf("clock error = %+v", c)
	}
}

func TestCollectRealHost(t *testing.T) {
	r := Collect(SystemSources())
	if r.Kernel == "" || r.CPUs == 0 || len(r.Checks) == 0 {
		t.Errorf("real host report looks empty: %+v", r)
	}
}

func TestCollectRocky9MissingCAKEIsInstallable(t *testing.T) {
	src := fakeHost()
	root := src.Root.(fstest.MapFS)
	root["etc/os-release"] = &fstest.MapFile{Data: []byte("ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"9.6\"\n")}
	src.Release = "5.14.0-570.el9.x86_64"
	root["lib/modules/"+src.Release+"/modules.dep"] = &fstest.MapFile{Data: []byte("kernel/net/sched/sch_netem.ko.xz:\n")}
	r := Collect(src)
	if cake := find(t, r, "CAKE"); !strings.Contains(cake.Fix, "kernel-modules-core") {
		t.Errorf("on EL9 a missing CAKE module should suggest the extra modules package, got %+v", cake)
	}
}
