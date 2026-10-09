package stun

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

const stunProfile = `
deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui: Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber
groups:
  - prefix: Device.DeviceInfo
    parameters:
      - path: Manufacturer
        value: "cpe-labs"
      - path: ManufacturerOUI
        value: "0000C5"
      - path: ProductClass
        value: "Test"
      - path: SerialNumber
        value: "SN1"
  - prefix: Device.ManagementServer
    parameters:
      - path: ConnectionRequestUsername
        value: "0000C5-SN1"
        writable: true
      - path: ConnectionRequestPassword
        value: "s3cret"
        writable: true
      - path: UDPConnectionRequestAddress
        value: ""
      - path: STUNEnable
        type: xsd:boolean
        value: "false"
        writable: true
      - path: STUNServerAddress
        value: ""
        writable: true
      - path: STUNServerPort
        type: xsd:unsignedInt
        value: "0"
        writable: true
      - path: STUNUsername
        value: ""
        writable: true
      - path: STUNPassword
        value: ""
        writable: true
      - path: STUNMaximumKeepAlivePeriod
        type: xsd:unsignedInt
        value: "0"
        writable: true
      - path: STUNMinimumKeepAlivePeriod
        type: xsd:unsignedInt
        value: "0"
        writable: true
      - path: NATDetected
        type: xsd:boolean
        value: "false"
`

func loadTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	prof, err := paramtree.LoadProfileFromReader(strings.NewReader(stunProfile), "stun.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return prof.Tree
}

// fakeServer is a STUN server on loopback that answers MAPPED-ADDRESS
// with the source, or with a fixed public address to stand in for a
// NAT, and records what the client sent.
type fakeServer struct {
	conn   *net.UDPConn
	mapped *net.UDPAddr // nil answers the real source

	mu       sync.Mutex
	requests []*Message
	from     *net.UDPAddr
}

func newFakeServer(t *testing.T, mapped *net.UDPAddr) *fakeServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{conn: conn, mapped: mapped}
	go s.serve()
	t.Cleanup(func() { _ = conn.Close() })
	return s
}

func (s *fakeServer) addr() *net.UDPAddr { return s.conn.LocalAddr().(*net.UDPAddr) }

func (s *fakeServer) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m, err := Decode(buf[:n])
		if err != nil || m.Type != TypeBindingRequest {
			continue
		}
		s.mu.Lock()
		s.requests = append(s.requests, m)
		s.from = from
		s.mu.Unlock()
		mapped := from
		if s.mapped != nil {
			mapped = s.mapped
		}
		resp := &Message{Type: TypeBindingResponse, TransactionID: m.TransactionID}
		v := make([]byte, 8)
		v[1] = 1
		binary.BigEndian.PutUint16(v[2:4], uint16(mapped.Port))
		copy(v[4:8], mapped.IP.To4())
		resp.Add(AttrMappedAddress, v)
		_, _ = s.conn.WriteToUDP(resp.Encode(), from)
	}
}

func (s *fakeServer) sentFrom() *net.UDPAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.from
}

func (s *fakeServer) firstRequest() *Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[0]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func enable(t *testing.T, tree *paramtree.Tree, server *net.UDPAddr) {
	t.Helper()
	for path, v := range map[string]string{
		"Device.ManagementServer.STUNServerAddress":          server.IP.String(),
		"Device.ManagementServer.STUNServerPort":             strconv.Itoa(server.Port),
		"Device.ManagementServer.STUNMinimumKeepAlivePeriod": "1",
		"Device.ManagementServer.STUNMaximumKeepAlivePeriod": "2",
		"Device.ManagementServer.STUNEnable":                 "true",
	} {
		if err := tree.SetSystem(path, v); err != nil {
			t.Fatal(err)
		}
	}
}

func get(t *testing.T, tree *paramtree.Tree, path string) string {
	t.Helper()
	v, err := tree.Get(path)
	if err != nil {
		t.Fatal(err)
	}
	return v.Raw
}

// Enabling STUN starts binding discovery: the first request carries
// CONNECTION-REQUEST-BINDING and BINDING-CHANGE, and what the server
// answers is written to the tree. Without a NAT the local address is
// reported and NATDetected is false (G.2.1.3).
func TestEnableRunsBindingDiscoveryAndReports(t *testing.T) {
	tree := loadTree(t)
	paths, err := Detect(tree)
	if err != nil {
		t.Fatal(err)
	}
	srv := newFakeServer(t, nil)
	c := New(Options{Tree: tree, Paths: paths, Logger: slog.Default()})
	stop := c.Start(context.Background())
	defer stop()

	if from := srv.sentFrom(); from != nil {
		t.Fatal("a client with STUNEnable false sent a request")
	}
	enable(t, tree, srv.addr())
	waitFor(t, "a binding report", func() bool { return get(t, tree, paths.UDPAddress) != "" })

	req := srv.firstRequest()
	if v, ok := req.Get(AttrConnectionRequestBinding); !ok || string(v) != ConnectionRequestBindingValue {
		t.Errorf("first request without CONNECTION-REQUEST-BINDING: %v", req.Attributes)
	}
	if _, ok := req.Get(AttrBindingChange); !ok {
		t.Error("first request without BINDING-CHANGE")
	}
	if _, ok := req.Get(AttrUsername); ok {
		t.Error("USERNAME sent with an empty STUNUsername")
	}
	if get(t, tree, paths.NATDetected) != "false" {
		t.Errorf("NATDetected %q on loopback", get(t, tree, paths.NATDetected))
	}
	if want := srv.sentFrom().String(); get(t, tree, paths.UDPAddress) != want {
		t.Errorf("UDPConnectionRequestAddress %q, want the local binding %s", get(t, tree, paths.UDPAddress), want)
	}
}

// Behind a NAT the server sees a different address, which is what the
// device reports, with NATDetected true.
func TestNATIsDetectedFromTheMappedAddress(t *testing.T) {
	tree := loadTree(t)
	paths, _ := Detect(tree)
	public := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 48942}
	srv := newFakeServer(t, public)
	c := New(Options{Tree: tree, Paths: paths, Logger: slog.Default()})
	defer c.Start(context.Background())()
	enable(t, tree, srv.addr())
	waitFor(t, "a binding report", func() bool { return get(t, tree, paths.UDPAddress) == "203.0.113.9:48942" })
	if get(t, tree, paths.NATDetected) != "true" {
		t.Error("NATDetected false behind a NAT")
	}
}

// A STUNUsername is sent padded to four bytes (G.2.1.3).
func TestUsernameIsPadded(t *testing.T) {
	tree := loadTree(t)
	paths, _ := Detect(tree)
	srv := newFakeServer(t, nil)
	if err := tree.SetSystem(paths.Username, "0000C5-SN1"); err != nil {
		t.Fatal(err)
	}
	c := New(Options{Tree: tree, Paths: paths, Logger: slog.Default()})
	defer c.Start(context.Background())()
	enable(t, tree, srv.addr())
	waitFor(t, "a request", func() bool { return srv.firstRequest() != nil })
	v, ok := srv.firstRequest().Get(AttrUsername)
	if !ok || string(v) != "0000C5-SN1  " {
		t.Errorf("USERNAME %q", v)
	}
}

func udpConnectionRequest(to *net.UDPAddr, username, password string, ts int64, id uint32) []byte {
	var cn [8]byte
	_, _ = rand.Read(cn[:])
	cnonce := strconv.FormatUint(binary.BigEndian.Uint64(cn[:]), 16)
	sig := Sign(password, ts, id, username, cnonce)
	authority := to.String()
	uri := "http://" + authority + "?ts=" + strconv.FormatInt(ts, 10) + "&id=" + strconv.FormatUint(uint64(id), 10) +
		"&un=" + username + "&cn=" + cnonce + "&sig=" + sig
	return []byte("GET " + uri + " HTTP/1.1\r\nHost: " + authority + "\r\n\r\n")
}

// A UDP Connection Request through the binding is validated as
// G.2.1.4 says and triggers a session; a replay, a wrong password and
// an older timestamp are ignored.
func TestConnectionRequestsAreValidated(t *testing.T) {
	tree := loadTree(t)
	paths, _ := Detect(tree)
	srv := newFakeServer(t, nil)
	var mu sync.Mutex
	sessions := 0
	c := New(Options{Tree: tree, Paths: paths, Logger: slog.Default(), OnConnectionRequest: func() {
		mu.Lock()
		sessions++
		mu.Unlock()
	}})
	defer c.Start(context.Background())()
	enable(t, tree, srv.addr())
	waitFor(t, "a binding report", func() bool { return get(t, tree, paths.UDPAddress) != "" })
	binding, err := net.ResolveUDPAddr("udp4", get(t, tree, paths.UDPAddress))
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return sessions
	}
	// Sent from the server's socket, the way the ACS does.
	send := func(b []byte) {
		if _, err := srv.conn.WriteToUDP(b, binding); err != nil {
			t.Fatal(err)
		}
	}
	ts := time.Now().Unix()
	good := udpConnectionRequest(binding, "0000C5-SN1", "s3cret", ts, 1)
	send(good)
	send(good)
	send(good)
	waitFor(t, "the session", func() bool { return count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if count() != 1 {
		t.Fatalf("three copies of one request ran %d sessions", count())
	}

	send(udpConnectionRequest(binding, "0000C5-SN1", "wrong", ts+1, 2))
	send(udpConnectionRequest(binding, "someone-else", "s3cret", ts+2, 3))
	send(udpConnectionRequest(binding, "0000C5-SN1", "s3cret", ts-5, 4))
	send([]byte("not a request at all"))
	time.Sleep(50 * time.Millisecond)
	if count() != 1 {
		t.Fatalf("an invalid request ran a session: %d", count())
	}

	send(udpConnectionRequest(binding, "0000C5-SN1", "s3cret", ts+3, 5))
	waitFor(t, "the second session", func() bool { return count() == 2 })
}

// Disabling STUN stops the loop; the client sends nothing more.
func TestDisableStopsTheClient(t *testing.T) {
	tree := loadTree(t)
	paths, _ := Detect(tree)
	srv := newFakeServer(t, nil)
	c := New(Options{Tree: tree, Paths: paths, Logger: slog.Default()})
	defer c.Start(context.Background())()
	enable(t, tree, srv.addr())
	waitFor(t, "a request", func() bool { return srv.firstRequest() != nil })
	if err := tree.SetSystem(paths.Enable, "false"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the loop to stop", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.running
	})
}

func TestDetect(t *testing.T) {
	tree := loadTree(t)
	p, err := Detect(tree)
	if err != nil || p.Enable != "Device.ManagementServer.STUNEnable" {
		t.Fatalf("Detect: %v %v", p, err)
	}
	empty, err := paramtree.LoadProfileFromReader(strings.NewReader(`
deviceIdPaths:
  manufacturer: Device.DeviceInfo.Manufacturer
  oui: Device.DeviceInfo.ManufacturerOUI
  productClass: Device.DeviceInfo.ProductClass
  serialNumber: Device.DeviceInfo.SerialNumber
groups:
  - prefix: Device.DeviceInfo
    parameters:
      - path: Manufacturer
        value: "x"
      - path: ManufacturerOUI
        value: "0000C5"
      - path: ProductClass
        value: "x"
      - path: SerialNumber
        value: "x"
`), "p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(empty.Tree); err != ErrNotModelled {
		t.Errorf("Detect on a profile without STUN: %v", err)
	}
}
