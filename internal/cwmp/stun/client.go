package stun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is what TR-069 Annex G specifies for the signature
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Paths are the ManagementServer leaves the client reads and writes,
// under whichever root the profile models.
type Paths struct {
	Enable, ServerAddress, ServerPort, Username, Password string
	MinKeepAlive, MaxKeepAlive                            string
	// UDPAddress and NATDetected are written by the client.
	UDPAddress, NATDetected string
	// CRUsername and CRPassword are the connection request credential
	// a UDP Connection Request is validated against.
	CRUsername, CRPassword string
}

// PathsUnder returns the standard leaf names under root, which ends
// with "ManagementServer.".
func PathsUnder(root string) Paths {
	return Paths{
		Enable:        root + "STUNEnable",
		ServerAddress: root + "STUNServerAddress",
		ServerPort:    root + "STUNServerPort",
		Username:      root + "STUNUsername",
		Password:      root + "STUNPassword",
		MinKeepAlive:  root + "STUNMinimumKeepAlivePeriod",
		MaxKeepAlive:  root + "STUNMaximumKeepAlivePeriod",
		UDPAddress:    root + "UDPConnectionRequestAddress",
		NATDetected:   root + "NATDetected",
		CRUsername:    root + "ConnectionRequestUsername",
		CRPassword:    root + "ConnectionRequestPassword",
	}
}

// Options configures a Client.
type Options struct {
	Tree  *paramtree.Tree
	Paths Paths
	// OnConnectionRequest runs for every validated UDP Connection
	// Request, on the client's goroutine; start a session elsewhere.
	OnConnectionRequest func()
	// ACSHost is the host of the ACS URL, the server when
	// STUNServerAddress is empty (G.2.1.1).
	ACSHost string
	Logger  *slog.Logger
}

// Client is one CPE's STUN client.
type Client struct {
	opts Options

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	// settings is what the loop was started with; a change restarts it.
	settings settings

	// Validation state of G.2.1.4, kept across restarts of the loop and
	// not across process restarts, which is what the Annex allows.
	lastTS int64
	lastID uint32
	seen   bool
}

type settings struct {
	enabled      bool
	server       string
	port         int
	username     string
	minKeepAlive time.Duration
	maxKeepAlive time.Duration
}

// New returns a client that does nothing until Start.
func New(opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{opts: opts}
}

// Start observes the tree and runs the binding loop whenever STUNEnable
// is true. It returns the function that stops everything.
func (c *Client) Start(ctx context.Context) (stop func()) {
	watched := map[string]bool{
		c.opts.Paths.Enable: true, c.opts.Paths.ServerAddress: true, c.opts.Paths.ServerPort: true,
		c.opts.Paths.Username: true, c.opts.Paths.Password: true,
		c.opts.Paths.MinKeepAlive: true, c.opts.Paths.MaxKeepAlive: true,
	}
	unobserve := c.opts.Tree.Observe(func(ch paramtree.Change) {
		if ch.Kind == paramtree.ChangeValue && watched[ch.Path] {
			// Observers run on the writer's goroutine; the restart
			// closes a socket and waits for its loop, so it goes aside.
			go c.reconcile(ctx)
		}
	})
	c.reconcile(ctx)
	return func() {
		unobserve()
		c.mu.Lock()
		if c.cancel != nil {
			c.cancel()
		}
		c.mu.Unlock()
	}
}

func (c *Client) read(path string) string {
	v, err := c.opts.Tree.Get(path)
	if err != nil {
		return ""
	}
	return v.Raw
}

func (c *Client) current() settings {
	s := settings{
		enabled:  isTrue(c.read(c.opts.Paths.Enable)),
		server:   strings.TrimSpace(c.read(c.opts.Paths.ServerAddress)),
		username: c.read(c.opts.Paths.Username),
	}
	if s.server == "" {
		s.server = c.opts.ACSHost
	}
	s.port, _ = strconv.Atoi(c.read(c.opts.Paths.ServerPort))
	if s.port <= 0 {
		s.port = 3478
	}
	minKA, _ := strconv.Atoi(c.read(c.opts.Paths.MinKeepAlive))
	maxKA, _ := strconv.Atoi(c.read(c.opts.Paths.MaxKeepAlive))
	if minKA <= 0 {
		minKA = 30
	}
	// The Annex leaves timeout discovery to the vendor; this vendor
	// keeps the binding alive at the minimum period, which is the
	// safest choice against an unknown NAT, bounded by the maximum when
	// one is set below it.
	if maxKA > 0 && maxKA < minKA {
		minKA = maxKA
	}
	s.minKeepAlive = time.Duration(minKA) * time.Second
	s.maxKeepAlive = time.Duration(maxKA) * time.Second
	return s
}

// reconcile starts, restarts or stops the loop to match the tree.
func (c *Client) reconcile(ctx context.Context) {
	want := c.current()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running && want == c.settings {
		return
	}
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
		c.running = false
	}
	if !want.enabled || want.server == "" {
		if want.enabled {
			c.opts.Logger.Warn("stun enabled with no server address and no ACS host; not starting")
		}
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.running = true
	c.settings = want
	go c.loop(loopCtx, want)
}

func isTrue(v string) bool {
	v = strings.TrimSpace(v)
	return strings.EqualFold(v, "true") || v == "1"
}

// loop is one run of the binding: discovery, keepalives, and the
// listener for connection requests, until the context ends.
func (c *Client) loop(ctx context.Context, s settings) {
	log := c.opts.Logger.With("stun_server", net.JoinHostPort(s.server, strconv.Itoa(s.port)))
	serverAddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(s.server, strconv.Itoa(s.port)))
	if err != nil {
		log.Warn("stun server address does not resolve", "err", err.Error())
		return
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		log.Warn("stun socket", "err", err.Error())
		return
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	local := localAddress(serverAddr, conn.LocalAddr().(*net.UDPAddr).Port)
	log.Info("stun client started", "local", local.String())

	// Binding discovery, then keepalives. The first request and every
	// one after a change carry BINDING-CHANGE (G.2.1.3); the request is
	// retransmitted on the RFC 3489 schedule until answered.
	var pending *pendingRequest
	var mapped *net.UDPAddr
	send := func(change bool) {
		m := &Message{Type: TypeBindingRequest}
		_, _ = rand.Read(m.TransactionID[:])
		if s.username != "" {
			m.Add(AttrUsername, PadUsername(s.username))
		}
		m.Add(AttrConnectionRequestBinding, []byte(ConnectionRequestBindingValue))
		if change {
			m.Add(AttrBindingChange, nil)
		}
		pending = &pendingRequest{msg: m, change: change, attempts: 0, next: time.Now()}
		pending.transmit(conn, serverAddr)
	}
	send(true)

	keepalive := time.NewTimer(s.minKeepAlive)
	defer keepalive.Stop()
	retry := time.NewTicker(100 * time.Millisecond)
	defer retry.Stop()
	datagrams := make(chan datagram, 16)
	go readDatagrams(conn, datagrams)

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			send(false)
			keepalive.Reset(s.minKeepAlive)
		case <-retry.C:
			if pending != nil && pending.due() {
				if pending.attempts >= 9 {
					log.Warn("stun binding request unanswered after 9 transmissions")
					pending = nil
					continue
				}
				pending.transmit(conn, serverAddr)
			}
		case d, ok := <-datagrams:
			if !ok {
				return
			}
			if !IsSTUN(d.data) {
				c.connectionRequest(d, log)
				continue
			}
			m, err := Decode(d.data)
			if err != nil || pending == nil || m.TransactionID != pending.msg.TransactionID {
				continue
			}
			switch m.Type {
			case TypeBindingErrorResponse:
				log.Warn("stun binding error response", "code", m.ErrorCode())
				pending = nil
			case TypeBindingResponse:
				pending = nil
				addr, err := m.MappedAddress()
				if err != nil {
					log.Warn("stun binding response", "err", err.Error())
					continue
				}
				nat := !addr.IP.Equal(local.IP) || addr.Port != local.Port
				if mapped == nil || !mapped.IP.Equal(addr.IP) || mapped.Port != addr.Port {
					if mapped != nil {
						// The binding moved: say so at once (G.2.1.3).
						send(true)
					}
					mapped = addr
					c.report(nat, addr, local, log)
				}
			}
		}
	}
}

// report writes what binding discovery found (G.2.1.3): the public
// address when a NAT is in the path, the local one when not.
func (c *Client) report(nat bool, mapped, local *net.UDPAddr, log *slog.Logger) {
	reported := local
	if nat {
		reported = mapped
	}
	value := net.JoinHostPort(reported.IP.String(), strconv.Itoa(reported.Port))
	if err := c.opts.Tree.SetSystem(c.opts.Paths.NATDetected, strconv.FormatBool(nat)); err != nil {
		log.Warn("write NATDetected", "err", err.Error())
	}
	if err := c.opts.Tree.SetSystem(c.opts.Paths.UDPAddress, value); err != nil {
		log.Warn("write UDPConnectionRequestAddress", "err", err.Error())
	}
	log.Info("stun binding", "nat_detected", nat, "udp_connection_request_address", value)
}

// connectionRequest validates a UDP Connection Request as G.2.1.4
// says: a GET, a timestamp later than the last accepted, a message id
// different from the last accepted, the connection request username,
// and the HMAC-SHA1 signature keyed with the connection request
// password. One that passes triggers a session; one that fails is
// ignored and leaves no record.
func (c *Client) connectionRequest(d datagram, log *slog.Logger) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(d.data)))
	if err != nil || req.Method != http.MethodGet {
		log.Debug("udp datagram is neither STUN nor a connection request", "from", d.from.String())
		return
	}
	q, _ := url.ParseQuery(req.URL.RawQuery)
	ts, err := strconv.ParseInt(q.Get("ts"), 10, 64)
	if err != nil {
		log.Debug("udp connection request without a timestamp", "from", d.from.String())
		return
	}
	id64, err := strconv.ParseUint(q.Get("id"), 10, 32)
	if err != nil {
		log.Debug("udp connection request without an id", "from", d.from.String())
		return
	}
	id := uint32(id64)
	username := c.read(c.opts.Paths.CRUsername)
	password := c.read(c.opts.Paths.CRPassword)

	c.mu.Lock()
	lastTS, lastID, seen := c.lastTS, c.lastID, c.seen
	c.mu.Unlock()
	switch {
	case ts <= lastTS:
		log.Info("udp connection request ignored: timestamp not later than the last accepted", "ts", ts, "last_ts", lastTS)
		return
	case seen && id == lastID:
		log.Info("udp connection request ignored: message id repeats the last accepted", "id", id)
		return
	case q.Get("un") != username:
		log.Info("udp connection request ignored: username mismatch", "un", q.Get("un"))
		return
	}
	want := Sign(password, ts, id, q.Get("un"), q.Get("cn"))
	if !strings.EqualFold(q.Get("sig"), want) {
		log.Info("udp connection request ignored: signature mismatch", "id", id)
		return
	}
	c.mu.Lock()
	c.lastTS, c.lastID, c.seen = ts, id, true
	c.mu.Unlock()
	log.Info("udp connection request accepted", "from", d.from.String(), "ts", ts, "id", id)
	if c.opts.OnConnectionRequest != nil {
		c.opts.OnConnectionRequest()
	}
}

// Sign is the Annex's signature: hex HMAC-SHA1 keyed with the
// connection request password over ts, id, username and cnonce.
func Sign(password string, ts int64, id uint32, username, cnonce string) string {
	mac := hmac.New(sha1.New, []byte(password))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + strconv.FormatUint(uint64(id), 10) + username + cnonce))
	return hex.EncodeToString(mac.Sum(nil))
}

type datagram struct {
	data []byte
	from *net.UDPAddr
}

func readDatagrams(conn *net.UDPConn, out chan<- datagram) {
	defer close(out)
	for {
		buf := make([]byte, 1500)
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		out <- datagram{data: buf[:n], from: from}
	}
}

// pendingRequest is a Binding Request awaiting its response, with the
// RFC 3489 section 9.3 retransmission schedule: 100 ms doubling to
// 1.6 s, then every 1.6 s, nine transmissions in all.
type pendingRequest struct {
	msg      *Message
	change   bool
	attempts int
	next     time.Time
}

func (p *pendingRequest) due() bool { return !time.Now().Before(p.next) }

func (p *pendingRequest) transmit(conn *net.UDPConn, to *net.UDPAddr) {
	_, _ = conn.WriteToUDP(p.msg.Encode(), to)
	p.attempts++
	wait := 100 * time.Millisecond << uint(p.attempts-1)
	if wait > 1600*time.Millisecond {
		wait = 1600 * time.Millisecond
	}
	p.next = time.Now().Add(wait)
}

// localAddress is the address the socket sends to the server from: the
// interface that routes there, found with a connected UDP socket that
// sends nothing, and the port the listening socket bound.
func localAddress(server *net.UDPAddr, port int) *net.UDPAddr {
	probe, err := net.DialUDP("udp4", nil, server)
	if err != nil {
		return &net.UDPAddr{IP: net.IPv4zero, Port: port}
	}
	defer probe.Close()
	ip := probe.LocalAddr().(*net.UDPAddr).IP
	return &net.UDPAddr{IP: ip, Port: port}
}

// ErrNotModelled is returned by Detect when the profile has no STUN
// leaves under either root.
var ErrNotModelled = errors.New("stun: the profile models no ManagementServer STUN leaves")

// Detect finds the root the profile models the STUN leaves under.
func Detect(tree *paramtree.Tree) (Paths, error) {
	for _, root := range []string{"Device.ManagementServer.", "InternetGatewayDevice.ManagementServer."} {
		p := PathsUnder(root)
		if _, err := tree.Get(p.Enable); err == nil {
			for _, required := range []string{p.ServerAddress, p.UDPAddress, p.NATDetected} {
				if _, err := tree.Get(required); err != nil {
					return Paths{}, fmt.Errorf("stun: %s is modelled but %s is not", p.Enable, required)
				}
			}
			return p, nil
		}
	}
	return Paths{}, ErrNotModelled
}
