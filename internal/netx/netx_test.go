package netx

import (
	"net"
	"syscall"
	"testing"
	"time"
)

// The kernel must report datagrams dropped by a full receive buffer.
func TestRecvInfoReportsOverflow(t *testing.T) {
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := EnableRecvInfo(l); err != nil {
		t.Fatal(err)
	}
	rc, _ := l.SyscallConn()
	_ = rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048) })
	s, err := net.DialUDP("udp4", nil, l.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = SetTOS(s, 0xb8)
	buf := make([]byte, 1000)
	for i := 0; i < 500; i++ { // far more than a 2 KiB buffer holds
		_, _ = s.Write(buf)
	}
	time.Sleep(50 * time.Millisecond)
	// Read one datagram to make room, then send more: these are queued
	// after drops and so carry the overflow count.
	_ = l.SetReadDeadline(time.Now().Add(time.Second))
	_, _, _, _, _ = l.ReadMsgUDP(make([]byte, 2000), make([]byte, OOBSize))
	for i := 0; i < 5; i++ {
		_, _ = s.Write(buf)
	}
	time.Sleep(50 * time.Millisecond)
	// The count rides on datagrams queued after the first drop: drain the
	// queue and keep the highest value seen.
	oob := make([]byte, OOBSize)
	var maxDrops uint32
	var first RecvInfo
	for i := 0; ; i++ {
		_ = l.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, oobn, _, _, err := l.ReadMsgUDP(buf, oob)
		if err != nil {
			break
		}
		ri := ParseRecvInfo(oob[:oobn])
		if i == 0 {
			first = ri // the first queued datagram (sent before any drop)
		}
		maxDrops = max(maxDrops, ri.Drops)
	}
	if maxDrops == 0 {
		t.Error("expected the kernel to report receive-buffer drops")
	}
	if first.TOS != 0xb8 || first.TTL != 64 || first.Time.IsZero() {
		t.Errorf("TOS/TTL/timestamp not reported: %+v", first)
	}
}

func TestTOSControl(t *testing.T) {
	if TOSControl(-1, true) != nil {
		t.Error("negative TOS must give no control message")
	}
	if len(TOSControl(0xb8, true)) == 0 || len(TOSControl(0xb8, false)) == 0 {
		t.Error("control message missing")
	}
	if OptionNames(OptTimestamps | OptSACK)[1] != "sack" {
		t.Error("option names")
	}
}
