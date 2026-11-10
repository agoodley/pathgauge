// Package netx wraps the Linux socket options PathGauge relies on:
// TCP_INFO, congestion control, pacing, TOS/DSCP, DF probing, kernel receive
// timestamps, received TOS/TTL and the ICMP error queue.
package netx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// control runs f on the connection's file descriptor.
func control(c syscall.Conn, f func(fd int) error) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// TCPInfo reads the kernel's TCP_INFO for a connection.
func TCPInfo(c syscall.Conn) (*unix.TCPInfo, error) {
	var info *unix.TCPInfo
	err := control(c, func(fd int) error {
		var err error
		info, err = unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
		return err
	})
	return info, err
}

// SetCongestion selects a congestion-control algorithm for one socket.
func SetCongestion(c syscall.Conn, alg string) error {
	if alg == "" {
		return nil
	}
	err := control(c, func(fd int) error {
		return unix.SetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_CONGESTION, alg)
	})
	if err != nil {
		return fmt.Errorf("congestion control %q: %w (is the module loaded, and do we have CAP_NET_ADMIN?)", alg, err)
	}
	return nil
}

// Congestion returns the socket's congestion-control algorithm.
func Congestion(c syscall.Conn) string {
	var alg string
	_ = control(c, func(fd int) error {
		var err error
		alg, err = unix.GetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_CONGESTION)
		return err
	})
	return alg
}

// SetPacingRate caps a socket's send rate in bytes per second (0 = off).
func SetPacingRate(c syscall.Conn, bytesPerSec uint64) error {
	if bytesPerSec == 0 {
		return nil
	}
	return control(c, func(fd int) error {
		return unix.SetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_MAX_PACING_RATE, bytesPerSec)
	})
}

// SetNotSentLowat limits how much unsent data the kernel holds for a TCP
// socket (TCP_NOTSENT_LOWAT), without limiting the data in flight. Writers
// then wait in the application instead of the socket buffer, so the
// application knows how far behind it is.
func SetNotSentLowat(c syscall.Conn, bytes int) error {
	return control(c, func(fd int) error {
		return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, bytes)
	})
}

// SetBuffers sets send and receive buffer sizes. With force it uses the
// *FORCE variants (needs CAP_NET_ADMIN) and falls back silently.
func SetBuffers(c syscall.Conn, size int) {
	if size <= 0 {
		return
	}
	_ = control(c, func(fd int) error {
		if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, size) != nil {
			_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, size)
		}
		if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, size) != nil {
			_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, size)
		}
		return nil
	})
}

// isV6 reports whether a socket is IPv6.
func isV6(fd int) bool {
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return false
	}
	_, ok := sa.(*unix.SockaddrInet6)
	return ok
}

// SetTOS sets the IPv4 TOS byte or IPv6 traffic class (DSCP<<2 | ECN).
func SetTOS(c syscall.Conn, tos int) error {
	return control(c, func(fd int) error {
		if isV6(fd) {
			return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_TCLASS, tos)
		}
		return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TOS, tos)
	})
}

// SetTTL sets the unicast TTL / hop limit.
func SetTTL(c syscall.Conn, ttl int) error {
	return control(c, func(fd int) error {
		if isV6(fd) {
			return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, ttl)
		}
		return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl)
	})
}

// EnableRecvInfo asks the kernel to report receive timestamps, TOS/traffic
// class and TTL/hop limit with each datagram.
func EnableRecvInfo(c syscall.Conn) error {
	return control(c, func(fd int) error {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1)
		// Report the socket's cumulative receive-buffer overflow count, so
		// loss caused by a slow receiving host is not blamed on the network.
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1)
		if isV6(fd) {
			_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVTCLASS, 1)
			_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVHOPLIMIT, 1)
			// Dual-stack sockets also receive IPv4 packets.
			_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVTOS, 1)
			_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVTTL, 1)
			return nil
		}
		_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVTOS, 1)
		return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVTTL, 1)
	})
}

// RecvInfo is per-datagram metadata from the kernel.
type RecvInfo struct {
	Time time.Time // kernel receive timestamp (zero if unavailable)
	TOS  int       // -1 if unavailable
	TTL  int       // -1 if unavailable
	// Drops is the socket's cumulative count of datagrams dropped because
	// its receive buffer was full (HasDrops false if unavailable).
	Drops    uint32
	HasDrops bool
}

// OOBSize is enough ancillary space for RecvInfo.
const OOBSize = 128

// ParseRecvInfo decodes ancillary data from ReadMsgUDP.
func ParseRecvInfo(oob []byte) RecvInfo {
	ri := RecvInfo{TOS: -1, TTL: -1}
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return ri
	}
	for _, m := range msgs {
		switch {
		case m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SO_RXQ_OVFL && len(m.Data) >= 4:
			ri.Drops, ri.HasDrops = binary.NativeEndian.Uint32(m.Data), true
		case m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_TIMESTAMPNS && len(m.Data) >= 16:
			sec := int64(binary.NativeEndian.Uint64(m.Data[0:8]))
			nsec := int64(binary.NativeEndian.Uint64(m.Data[8:16]))
			ri.Time = time.Unix(sec, nsec)
		case m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_TOS && len(m.Data) >= 1:
			ri.TOS = int(m.Data[0])
		case m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_TTL && len(m.Data) >= 4:
			ri.TTL = int(binary.NativeEndian.Uint32(m.Data))
		case m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_TCLASS && len(m.Data) >= 4:
			ri.TOS = int(binary.NativeEndian.Uint32(m.Data))
		case m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_HOPLIMIT && len(m.Data) >= 4:
			ri.TTL = int(binary.NativeEndian.Uint32(m.Data))
		}
	}
	return ri
}

// SetDFProbe sets Don't Fragment and ignores the cached path MTU, so
// oversized probes are sent as-is (IP_PMTUDISC_PROBE). It also enables
// the error queue so ICMP "fragmentation needed" can be read.
func SetDFProbe(c syscall.Conn) error {
	return control(c, func(fd int) error {
		if isV6(fd) {
			// Dual-stack sockets also carry IPv4: set both families.
			_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1)
			_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE)
			_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVERR, 1)
			return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE)
		}
		_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1)
		return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE)
	})
}

// EnableRecvErr turns on the ICMP error queue (IP_RECVERR).
func EnableRecvErr(c syscall.Conn) error {
	return control(c, func(fd int) error {
		if isV6(fd) {
			return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVERR, 1)
		}
		return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1)
	})
}

// SockErr is one entry from the socket error queue.
type SockErr struct {
	Errno    syscall.Errno
	Origin   uint8 // SO_EE_ORIGIN_*
	Type     uint8 // ICMP type
	Code     uint8 // ICMP code
	Info     uint32
	Offender netip.Addr // router or host that sent the ICMP
	Time     time.Time
	Payload  []byte // start of the original datagram's payload, when quoted
}

// IsTimeExceeded reports an ICMP(v6) time exceeded / hop limit error.
func (e SockErr) IsTimeExceeded() bool {
	return (e.Origin == unix.SO_EE_ORIGIN_ICMP && e.Type == 11) || (e.Origin == unix.SO_EE_ORIGIN_ICMP6 && e.Type == 3)
}

// IsUnreachable reports an ICMP(v6) destination unreachable.
func (e SockErr) IsUnreachable() bool {
	return (e.Origin == unix.SO_EE_ORIGIN_ICMP && e.Type == 3) || (e.Origin == unix.SO_EE_ORIGIN_ICMP6 && e.Type == 1)
}

// IsPortUnreachable reports an ICMP(v6) port unreachable: the packet
// reached a host with nothing listening on that port.
func (e SockErr) IsPortUnreachable() bool {
	return (e.Origin == unix.SO_EE_ORIGIN_ICMP && e.Type == 3 && e.Code == 3) || (e.Origin == unix.SO_EE_ORIGIN_ICMP6 && e.Type == 1 && e.Code == 4)
}

// ErrNoError means the error queue was empty.
var ErrNoError = errors.New("error queue empty")

// ReadErrQueue reads one entry from the error queue without blocking.
func ReadErrQueue(c syscall.Conn) (SockErr, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return SockErr{}, err
	}
	var se SockErr
	var rerr error
	buf := make([]byte, 512)
	oob := make([]byte, 512)
	cerr := rc.Control(func(fd uintptr) {
		n, oobn, _, _, err := unix.Recvmsg(int(fd), buf, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) {
				rerr = ErrNoError
			} else {
				rerr = err
			}
			return
		}
		se, rerr = parseErrQueue(oob[:oobn])
		se.Payload = append([]byte(nil), buf[:n]...)
	})
	if cerr != nil {
		return SockErr{}, cerr
	}
	return se, rerr
}

func parseErrQueue(oob []byte) (SockErr, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return SockErr{}, err
	}
	se := SockErr{Time: time.Now()}
	found := false
	for _, m := range msgs {
		isErr := (m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_RECVERR) ||
			(m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_RECVERR)
		switch {
		case isErr && len(m.Data) >= int(unsafe.Sizeof(unix.SockExtendedErr{})):
			ee := (*unix.SockExtendedErr)(unsafe.Pointer(&m.Data[0]))
			se.Errno, se.Origin, se.Type, se.Code, se.Info = syscall.Errno(ee.Errno), ee.Origin, ee.Type, ee.Code, ee.Info
			off := m.Data[unsafe.Sizeof(unix.SockExtendedErr{}):]
			se.Offender = parseSockaddr(off)
			found = true
		case m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_TIMESTAMPNS && len(m.Data) >= 16:
			se.Time = time.Unix(int64(binary.NativeEndian.Uint64(m.Data[0:8])), int64(binary.NativeEndian.Uint64(m.Data[8:16])))
		}
	}
	if !found {
		return se, errors.New("no extended error in control message")
	}
	return se, nil
}

// parseSockaddr decodes a raw sockaddr_in / sockaddr_in6.
func parseSockaddr(b []byte) netip.Addr {
	if len(b) < 2 {
		return netip.Addr{}
	}
	switch binary.NativeEndian.Uint16(b[0:2]) {
	case unix.AF_INET:
		if len(b) >= 8 {
			return netip.AddrFrom4([4]byte(b[4:8]))
		}
	case unix.AF_INET6:
		if len(b) >= 24 {
			return netip.AddrFrom16([16]byte(b[8:24]))
		}
	}
	return netip.Addr{}
}

// PathMTU returns the kernel's cached path MTU for a connected socket.
func PathMTU(c syscall.Conn) int {
	mtu := 0
	_ = control(c, func(fd int) error {
		var err error
		if isV6(fd) {
			mtu, err = unix.GetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MTU)
		} else {
			mtu, err = unix.GetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MTU)
		}
		return err
	})
	return mtu
}

// HeaderOverhead returns IP+UDP header bytes for an address family.
func HeaderOverhead(addr net.Addr) int {
	if ua, ok := addr.(*net.UDPAddr); ok && ua.IP.To4() == nil {
		return 40 + 8
	}
	return 20 + 8
}

// TCPInfoSummary is the subset of TCP_INFO PathGauge reports.
type TCPInfoSummary struct {
	RTTus         uint32 `json:"rtt_us"`
	RTTVarUs      uint32 `json:"rttvar_us"`
	MinRTTus      uint32 `json:"min_rtt_us"`
	Cwnd          uint32 `json:"cwnd"`
	SndMSS        uint32 `json:"snd_mss"`
	RcvMSS        uint32 `json:"rcv_mss"`
	AdvMSS        uint32 `json:"adv_mss"`
	PMTU          uint32 `json:"pmtu"`
	TotalRetrans  uint32 `json:"total_retrans"`
	BytesAcked    uint64 `json:"bytes_acked"`
	BytesReceived uint64 `json:"bytes_received"`
	BytesRetrans  uint64 `json:"bytes_retrans"`
	DeliveryRate  uint64 `json:"delivery_rate"` // bytes/s
	PacingRate    uint64 `json:"pacing_rate"`   // bytes/s
	BusyUs        uint64 `json:"busy_us"`
	RwndLimitedUs uint64 `json:"rwnd_limited_us"`
	SndbufLimUs   uint64 `json:"sndbuf_limited_us"`
	Options       uint8  `json:"options"`
	SndWnd        uint32 `json:"snd_wnd"`
	RcvWnd        uint32 `json:"rcv_wnd"`
	DeliveredCE   uint32 `json:"delivered_ce"`
}

// Summarize converts a raw TCP_INFO.
func Summarize(i *unix.TCPInfo) TCPInfoSummary {
	if i == nil {
		return TCPInfoSummary{}
	}
	return TCPInfoSummary{
		RTTus: i.Rtt, RTTVarUs: i.Rttvar, MinRTTus: i.Min_rtt, Cwnd: i.Snd_cwnd,
		SndMSS: i.Snd_mss, RcvMSS: i.Rcv_mss, AdvMSS: i.Advmss, PMTU: i.Pmtu,
		TotalRetrans: i.Total_retrans, BytesAcked: i.Bytes_acked, BytesReceived: i.Bytes_received,
		BytesRetrans: i.Bytes_retrans, DeliveryRate: i.Delivery_rate, PacingRate: i.Pacing_rate,
		BusyUs: i.Busy_time, RwndLimitedUs: i.Rwnd_limited, SndbufLimUs: i.Sndbuf_limited,
		Options: i.Options, SndWnd: i.Snd_wnd, RcvWnd: i.Rcv_wnd, DeliveredCE: i.Delivered_ce,
	}
}

// TCP option flags in tcpi_options.
const (
	OptTimestamps = 1
	OptSACK       = 2
	OptWScale     = 4
	OptECN        = 8
)

// OptionNames lists the negotiated TCP options.
func OptionNames(o uint8) []string {
	var out []string
	for _, x := range []struct {
		bit  uint8
		name string
	}{{OptTimestamps, "timestamps"}, {OptSACK, "sack"}, {OptWScale, "window_scaling"}, {OptECN, "ecn"}} {
		if o&x.bit != 0 {
			out = append(out, x.name)
		}
	}
	return out
}

// TOSControl builds ancillary data that sets the TOS / traffic class of
// one outgoing datagram. v4 selects IP_TOS (also correct for IPv4-mapped
// destinations on dual-stack sockets); otherwise IPV6_TCLASS.
func TOSControl(tos int, v4 bool) []byte {
	if tos < 0 {
		return nil
	}
	b := make([]byte, unix.CmsgSpace(4))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	if v4 {
		h.Level, h.Type = unix.IPPROTO_IP, unix.IP_TOS
	} else {
		h.Level, h.Type = unix.IPPROTO_IPV6, unix.IPV6_TCLASS
	}
	h.SetLen(unix.CmsgLen(4))
	binary.NativeEndian.PutUint32(b[unix.CmsgLen(0):], uint32(tos))
	return b
}

// IsV4 reports whether a UDP address is IPv4 or IPv4-mapped.
func IsV4(a *net.UDPAddr) bool { return a != nil && a.IP.To4() != nil }

// InterfaceMTU returns the MTU of the interface holding a local address
// (0 if unknown).
func InterfaceMTU(local net.IP) int {
	ifs, err := net.Interfaces()
	if err != nil {
		return 0
	}
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local) {
				return ifc.MTU
			}
		}
	}
	return 0
}
