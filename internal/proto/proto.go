// Package proto defines PathGauge's data-plane wire formats.
//
// One port (default 7400) carries everything, over TCP and UDP:
//
//   - A TCP control connection starts with "PGv1" 'C', then exchanges
//     length-prefixed JSON messages: a mutual HMAC handshake, then
//     request/response operations and asynchronous events.
//   - A TCP data connection starts with "PGv1" 'D' and a DataHeader that
//     binds it to an authenticated session and a stream ID.
//   - UDP packets use a fixed 64-byte header (UDPHeader) with four
//     timestamps and a truncated HMAC, optionally followed by padding.
//
// Every connection and packet must authenticate with a key derived from
// a shared secret, so an endpoint can never be used by strangers as a
// traffic source, and reflected UDP packets are never larger than the
// packet that caused them (no amplification).
package proto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the wire protocol version.
const Version = 1

// Connection preambles.
const (
	Magic       = "PGv1"
	ConnControl = 'C'
	ConnData    = 'D'
	// MaxMsg bounds a control message.
	MaxMsg = 16 << 20
)

// Msg is the control-channel envelope.
type Msg struct {
	Type  string          `json:"type"` // hello, challenge, auth, welcome, req, resp, event
	ID    uint64          `json:"id,omitempty"`
	Op    string          `json:"op,omitempty"`
	OK    bool            `json:"ok,omitempty"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// WriteMsg writes one length-prefixed JSON message.
func WriteMsg(w io.Writer, m *Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxMsg {
		return fmt.Errorf("message too large (%d bytes)", len(b))
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadMsg reads one length-prefixed JSON message.
func ReadMsg(r io.Reader, m *Msg) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxMsg {
		return fmt.Errorf("message too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	*m = Msg{}
	return json.Unmarshal(b, m)
}

// Encode marshals v into a Msg's Data field.
func Encode(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only our own types are encoded
	}
	return b
}

// Hello is the client's first control message.
type Hello struct {
	Version     int    `json:"version"`
	KeyID       string `json:"key_id"`
	ClientNonce []byte `json:"client_nonce"`
	LocalAddr   string `json:"local_addr"`
	Software    string `json:"software"`
}

// Challenge is the endpoint's reply to Hello.
type Challenge struct {
	Version     int    `json:"version"`
	ServerNonce []byte `json:"server_nonce"`
	// ObservedAddr is the client's address as seen by the endpoint; it
	// differs from the client's local address behind NAT.
	ObservedAddr string `json:"observed_addr"`
	Software     string `json:"software"`
}

// Auth proves the client knows the key.
type Auth struct {
	MAC []byte `json:"mac"`
}

// Welcome completes the handshake and proves the endpoint knows the key.
type Welcome struct {
	SessionID string `json:"session_id"` // hex, 16 bytes
	MAC       []byte `json:"mac"`
}

// NewNonce returns 16 random bytes.
func NewNonce() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func mac(key []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// ClientMAC is the client's handshake proof.
func ClientMAC(key, clientNonce, serverNonce []byte) []byte {
	return mac(key, []byte("pathgauge-auth-client|"), clientNonce, serverNonce)
}

// ServerMAC is the endpoint's handshake proof.
func ServerMAC(key, clientNonce, serverNonce []byte) []byte {
	return mac(key, []byte("pathgauge-auth-server|"), clientNonce, serverNonce)
}

// SessionKey derives the per-session key used for data headers and UDP.
func SessionKey(key, clientNonce, serverNonce []byte) []byte {
	return mac(key, []byte("pathgauge-session|"), clientNonce, serverNonce)
}

// Equal compares MACs in constant time.
func Equal(a, b []byte) bool { return hmac.Equal(a, b) }

// SessionID is a 16-byte session identifier.
type SessionID [16]byte

// NewSessionID returns a random session ID.
func NewSessionID() SessionID {
	var id SessionID
	copy(id[:], NewNonce())
	return id
}

func (s SessionID) String() string { return hex.EncodeToString(s[:]) }

// Short returns the 8-byte form carried in UDP packets.
func (s SessionID) Short() uint64 { return binary.BigEndian.Uint64(s[:8]) }

// ParseSessionID parses the hex form.
func ParseSessionID(s string) (SessionID, error) {
	var id SessionID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, errors.New("invalid session id")
	}
	copy(id[:], b)
	return id, nil
}

// DataHeaderLen is the size of a data connection header after the
// 5-byte preamble.
const DataHeaderLen = 16 + 4 + 16 + 16

// DataHeader binds a TCP data connection to a session stream.
type DataHeader struct {
	Session SessionID
	Stream  uint32
	Nonce   [16]byte
	MAC     [16]byte
}

// NewDataHeader builds an authenticated header.
func NewDataHeader(session SessionID, stream uint32, sessionKey []byte) DataHeader {
	h := DataHeader{Session: session, Stream: stream}
	copy(h.Nonce[:], NewNonce())
	copy(h.MAC[:], h.expectedMAC(sessionKey))
	return h
}

func (h DataHeader) expectedMAC(sessionKey []byte) []byte {
	var sb [4]byte
	binary.BigEndian.PutUint32(sb[:], h.Stream)
	return mac(sessionKey, []byte("pathgauge-data|"), h.Session[:], sb[:], h.Nonce[:])[:16]
}

// Valid checks the header's MAC.
func (h DataHeader) Valid(sessionKey []byte) bool {
	return hmac.Equal(h.MAC[:], h.expectedMAC(sessionKey))
}

// Marshal encodes the header including the preamble.
func (h DataHeader) Marshal() []byte {
	b := make([]byte, 0, 5+DataHeaderLen)
	b = append(b, Magic...)
	b = append(b, ConnData)
	b = append(b, h.Session[:]...)
	b = binary.BigEndian.AppendUint32(b, h.Stream)
	b = append(b, h.Nonce[:]...)
	return append(b, h.MAC[:]...)
}

// UnmarshalDataHeader decodes the bytes following the preamble.
func UnmarshalDataHeader(b []byte) (DataHeader, error) {
	var h DataHeader
	if len(b) != DataHeaderLen {
		return h, errors.New("short data header")
	}
	copy(h.Session[:], b[0:16])
	h.Stream = binary.BigEndian.Uint32(b[16:20])
	copy(h.Nonce[:], b[20:36])
	copy(h.MAC[:], b[36:52])
	return h, nil
}
