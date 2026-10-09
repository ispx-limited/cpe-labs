// Package stun is the CPE side of TR-069 Annex G: the classic STUN
// (RFC 3489) client a simulated CPE keeps its NAT binding open with,
// the two attributes the Annex adds to its Binding Requests, and the
// listener for the UDP Connection Requests the ACS sends back through
// the binding.
//
// The client is driven by the parameter tree the way a real device
// is: it starts when the ACS writes STUNEnable true, sends to the
// server in STUNServerAddress and STUNServerPort, keeps the binding
// alive inside the keepalive bounds, and reports what it found in
// UDPConnectionRequestAddress and NATDetected. A UDP Connection Request
// that validates (G.2.1.4) triggers a session exactly as an HTTP one
// does.
package stun

import (
	"encoding/binary"
	"errors"
	"net"
)

// Message types (RFC 3489 section 11.1).
const (
	TypeBindingRequest       uint16 = 0x0001
	TypeBindingResponse      uint16 = 0x0101
	TypeBindingErrorResponse uint16 = 0x0111
)

// Attribute types: RFC 3489 section 11.2 and TR-069 Annex G table 103.
const (
	AttrMappedAddress            uint16 = 0x0001
	AttrUsername                 uint16 = 0x0006
	AttrMessageIntegrity         uint16 = 0x0008
	AttrErrorCode                uint16 = 0x0009
	AttrConnectionRequestBinding uint16 = 0xC001
	AttrBindingChange            uint16 = 0xC002
)

// ConnectionRequestBindingValue is the fixed 20 byte value of
// CONNECTION-REQUEST-BINDING, trailing space included.
const ConnectionRequestBindingValue = "dslforum.org/TR-111 "

const headerLen = 20

// Attribute is one TLV, value unpadded.
type Attribute struct {
	Type  uint16
	Value []byte
}

// Message is a STUN message.
type Message struct {
	Type          uint16
	TransactionID [16]byte
	Attributes    []Attribute
}

// ErrMalformed is a datagram that is not a STUN message.
var ErrMalformed = errors.New("stun: malformed message")

// IsSTUN reports whether a datagram can be STUN: the first byte is 0
// or 1, where a UDP Connection Request starts with an ASCII letter.
func IsSTUN(b []byte) bool {
	return len(b) >= headerLen && b[0]&0xC0 == 0
}

// Decode parses a datagram.
func Decode(b []byte) (*Message, error) {
	if len(b) < headerLen {
		return nil, ErrMalformed
	}
	m := &Message{Type: binary.BigEndian.Uint16(b[0:2])}
	length := int(binary.BigEndian.Uint16(b[2:4]))
	copy(m.TransactionID[:], b[4:20])
	if headerLen+length > len(b) {
		return nil, ErrMalformed
	}
	body := b[headerLen : headerLen+length]
	for len(body) > 0 {
		if len(body) < 4 {
			return nil, ErrMalformed
		}
		t := binary.BigEndian.Uint16(body[0:2])
		l := int(binary.BigEndian.Uint16(body[2:4]))
		if 4+l > len(body) {
			return nil, ErrMalformed
		}
		m.Attributes = append(m.Attributes, Attribute{Type: t, Value: append([]byte(nil), body[4:4+l]...)})
		next := 4 + l
		if rem := next % 4; rem != 0 {
			next += 4 - rem
		}
		if next > len(body) {
			next = len(body)
		}
		body = body[next:]
	}
	return m, nil
}

// Encode serialises the message. Values are padded to four bytes with
// zeros; USERNAME is padded by the caller with spaces, as the Annex
// says, so its stated length is already a multiple of four.
func (m *Message) Encode() []byte {
	out := make([]byte, headerLen, headerLen+64)
	binary.BigEndian.PutUint16(out[0:2], m.Type)
	copy(out[4:20], m.TransactionID[:])
	for _, a := range m.Attributes {
		var tl [4]byte
		binary.BigEndian.PutUint16(tl[0:2], a.Type)
		binary.BigEndian.PutUint16(tl[2:4], uint16(len(a.Value)))
		out = append(out, tl[:]...)
		out = append(out, a.Value...)
		if rem := len(a.Value) % 4; rem != 0 {
			out = append(out, make([]byte, 4-rem)...)
		}
	}
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)-headerLen))
	return out
}

// Get returns the first attribute of the type.
func (m *Message) Get(t uint16) ([]byte, bool) {
	for _, a := range m.Attributes {
		if a.Type == t {
			return a.Value, true
		}
	}
	return nil, false
}

// Add appends an attribute.
func (m *Message) Add(t uint16, v []byte) {
	m.Attributes = append(m.Attributes, Attribute{Type: t, Value: v})
}

// MappedAddress reads MAPPED-ADDRESS (RFC 3489 section 11.2.1): a zero
// byte, the family, the port, the address.
func (m *Message) MappedAddress() (*net.UDPAddr, error) {
	v, ok := m.Get(AttrMappedAddress)
	if !ok {
		return nil, errors.New("stun: no MAPPED-ADDRESS in the response")
	}
	if len(v) < 8 || v[1] != 0x01 {
		return nil, ErrMalformed
	}
	return &net.UDPAddr{IP: net.IPv4(v[4], v[5], v[6], v[7]), Port: int(binary.BigEndian.Uint16(v[2:4]))}, nil
}

// ErrorCode reads ERROR-CODE's number, 0 when absent.
func (m *Message) ErrorCode() int {
	v, ok := m.Get(AttrErrorCode)
	if !ok || len(v) < 4 {
		return 0
	}
	return int(v[2]&0x07)*100 + int(v[3])
}

// PadUsername pads a STUNUsername with trailing spaces to a multiple
// of four bytes, as Annex G G.2.1.3 requires.
func PadUsername(u string) []byte {
	b := []byte(u)
	for len(b)%4 != 0 {
		b = append(b, ' ')
	}
	return b
}
