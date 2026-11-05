package proto

import (
	"bytes"
	"testing"
)

func TestMsgRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &Msg{Type: "req", ID: 7, Op: "info", Data: Encode(map[string]int{"x": 1})}
	if err := WriteMsg(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out Msg
	if err := ReadMsg(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "req" || out.ID != 7 || out.Op != "info" || string(out.Data) != `{"x":1}` {
		t.Errorf("got %+v", out)
	}
}

func TestReadMsgRejectsHuge(t *testing.T) {
	b := []byte{0xff, 0xff, 0xff, 0xff}
	var m Msg
	if err := ReadMsg(bytes.NewReader(b), &m); err == nil {
		t.Error("expected size error")
	}
}

func TestHandshakeMACs(t *testing.T) {
	key := []byte("secret")
	cn, sn := NewNonce(), NewNonce()
	if Equal(ClientMAC(key, cn, sn), ServerMAC(key, cn, sn)) {
		t.Error("client and server MACs must differ (no reflection)")
	}
	if !Equal(ClientMAC(key, cn, sn), ClientMAC(key, cn, sn)) {
		t.Error("deterministic")
	}
	if Equal(ClientMAC(key, cn, sn), ClientMAC([]byte("other"), cn, sn)) {
		t.Error("key must matter")
	}
	if bytes.Equal(SessionKey(key, cn, sn), key) {
		t.Error("session key must be derived")
	}
}

func TestDataHeader(t *testing.T) {
	key := []byte("session-key")
	sid := NewSessionID()
	h := NewDataHeader(sid, 3, key)
	b := h.Marshal()
	if string(b[:4]) != Magic || b[4] != ConnData || len(b) != 5+DataHeaderLen {
		t.Fatalf("bad preamble/length %d", len(b))
	}
	got, err := UnmarshalDataHeader(b[5:])
	if err != nil || got.Session != sid || got.Stream != 3 || !got.Valid(key) {
		t.Errorf("round trip failed: %+v %v", got, err)
	}
	got.Stream = 4
	if got.Valid(key) {
		t.Error("tampered stream must fail")
	}
	if h.Valid([]byte("wrong")) {
		t.Error("wrong key must fail")
	}
	if p, err := ParseSessionID(sid.String()); err != nil || p != sid {
		t.Error("session id parse")
	}
}

func TestUDPPacket(t *testing.T) {
	key := []byte("k")
	s := NewSigner(key)
	b := make([]byte, 200)
	h := UDPHeader{Type: UDPProbe, Session: 42, Seq: 9, Stream: 1, T1: 123456789, TOSSent: 0xb8}
	s.Encode(b, &h)
	if !s.Verify(b) {
		t.Fatal("verify failed")
	}
	sess, err := PeekSession(b)
	if err != nil || sess != 42 {
		t.Error("peek")
	}
	d, err := DecodeUDP(b)
	if err != nil || d.Seq != 9 || d.T1 != 123456789 || d.Length != 200 || d.TOSSent != 0xb8 || d.Type != UDPProbe {
		t.Errorf("decode %+v %v", d, err)
	}
	b[20] ^= 1
	if s.Verify(b) {
		t.Error("tampered packet verified")
	}
	if NewSigner([]byte("other")).Verify(b) {
		t.Error("wrong key verified")
	}
	if _, err := PeekSession([]byte("short")); err == nil {
		t.Error("short packet accepted")
	}
}
