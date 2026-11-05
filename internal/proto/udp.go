package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
)

// UDP packet types.
const (
	UDPProbe     byte = 1 // client → endpoint, reflected as UDPReply
	UDPReply     byte = 2 // reflected probe with t2/t3 filled in
	UDPData      byte = 3 // one-way test stream packet
	UDPPunch     byte = 4 // opens/refreshes the NAT binding for reverse streams
	UDPPMTU      byte = 5 // DF-set path MTU probe
	UDPPMTUAck   byte = 6 // small acknowledgement of a PMTU probe
	UDPKeepalive byte = 7
)

// UDPHeaderLen is the fixed header size, which is also the minimum packet.
const UDPHeaderLen = 64

const udpMagic = "PGU1"

// FlagReflected marks packets sent by the endpoint in reply.
const FlagReflected = 1

// UDPHeader is the fixed part of every UDP packet.
//
//	0  magic "PGU1"   4  type   5 flags   6 tos sent   7 reserved
//	8  session (short, 8 bytes)
//	16 sequence (8)   24 stream (4)   28 total length (4)
//	32 t1 sender transmit (unix ns)
//	40 t2 endpoint receive (unix ns)
//	48 t3 endpoint transmit (unix ns)
//	56 HMAC-SHA256 over bytes 0..55, truncated to 8 bytes
type UDPHeader struct {
	Type    byte
	Flags   byte
	TOSSent byte
	Session uint64
	Seq     uint64
	Stream  uint32
	Length  uint32
	T1      int64
	T2      int64
	T3      int64
}

// Signer computes and checks UDP MACs; it is not safe for concurrent use.
type Signer struct {
	h   hash.Hash
	sum []byte
}

// NewSigner returns a Signer for a session key.
func NewSigner(sessionKey []byte) *Signer {
	return &Signer{h: hmac.New(sha256.New, sessionKey)}
}

func (s *Signer) mac(b []byte) []byte {
	s.h.Reset()
	s.h.Write(b[:56])
	s.sum = s.h.Sum(s.sum[:0])
	return s.sum[:8]
}

// Encode writes h into b (len(b) >= UDPHeaderLen, the packet size) and
// signs it. Bytes after the header are left untouched (padding).
func (s *Signer) Encode(b []byte, h *UDPHeader) {
	h.Length = uint32(len(b))
	copy(b[0:4], udpMagic)
	b[4], b[5], b[6], b[7] = h.Type, h.Flags, h.TOSSent, 0
	binary.BigEndian.PutUint64(b[8:16], h.Session)
	binary.BigEndian.PutUint64(b[16:24], h.Seq)
	binary.BigEndian.PutUint32(b[24:28], h.Stream)
	binary.BigEndian.PutUint32(b[28:32], h.Length)
	binary.BigEndian.PutUint64(b[32:40], uint64(h.T1))
	binary.BigEndian.PutUint64(b[40:48], uint64(h.T2))
	binary.BigEndian.PutUint64(b[48:56], uint64(h.T3))
	copy(b[56:64], s.mac(b))
}

// Verify checks the MAC of an encoded packet.
func (s *Signer) Verify(b []byte) bool {
	return len(b) >= UDPHeaderLen && hmac.Equal(b[56:64], s.mac(b))
}

// ErrNotPathGauge marks packets that are not PathGauge UDP packets.
var ErrNotPathGauge = errors.New("not a PathGauge packet")

// PeekSession returns the session short ID without verifying the MAC, so
// the receiver can find the right key.
func PeekSession(b []byte) (uint64, error) {
	if len(b) < UDPHeaderLen || string(b[0:4]) != udpMagic {
		return 0, ErrNotPathGauge
	}
	return binary.BigEndian.Uint64(b[8:16]), nil
}

// DecodeUDP parses the header (call Verify first).
func DecodeUDP(b []byte) (UDPHeader, error) {
	if len(b) < UDPHeaderLen || string(b[0:4]) != udpMagic {
		return UDPHeader{}, ErrNotPathGauge
	}
	return UDPHeader{
		Type:    b[4],
		Flags:   b[5],
		TOSSent: b[6],
		Session: binary.BigEndian.Uint64(b[8:16]),
		Seq:     binary.BigEndian.Uint64(b[16:24]),
		Stream:  binary.BigEndian.Uint32(b[24:28]),
		Length:  binary.BigEndian.Uint32(b[28:32]),
		T1:      int64(binary.BigEndian.Uint64(b[32:40])),
		T2:      int64(binary.BigEndian.Uint64(b[40:48])),
		T3:      int64(binary.BigEndian.Uint64(b[48:56])),
	}, nil
}
