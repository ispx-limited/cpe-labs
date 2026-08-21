package mqtt

import (
	"errors"
	"net"
	"sync"
	"time"
)

// A simulated uplink between the agent and the broker.
//
// WHY NOT JUST DISCONNECT.
//
// Client.Disconnect sends an MQTT DISCONNECT packet, and a broker that
// receives one knows immediately that the session is over. A CPE whose
// WAN has failed sends nothing at all: no DISCONNECT, no FIN, no RST.
// The broker keeps the session open and only gives up once the
// keepalive lapses, which is why an outage takes a keepalive and a half
// to become visible instead of being visible at once.
//
// Reproducing that is the whole point of the fault, so the link is cut
// on the AGENT side only: reads fail, writes go nowhere, and the socket
// is deliberately left open until the fault ends. The broker is told
// nothing and has to notice by itself, exactly as it would if the fibre
// had been cut.
//
// Every dial is refused while the link is down too, so the agent's own
// reconnect loop keeps trying and getting nowhere, which is what a CPE
// with no route does.

// ErrLinkDown is what the MQTT library sees for every read and every
// dial attempt while the simulated uplink is down.
var ErrLinkDown = errors.New("usp/mqtt: simulated uplink is down")

// link is one agent's simulated uplink. The zero value is a working
// link, so an agent that never faults pays nothing for this.
type link struct {
	mu   sync.Mutex
	down bool

	// live are the sockets the MQTT library currently owns. held are
	// sockets cut by an outage: kept open rather than closed, because
	// closing one sends a FIN and a FIN is precisely the notice a
	// failed uplink cannot give. They are closed when the link
	// returns, by which time the broker has timed the session out on
	// its own.
	live []net.Conn
	held []net.Conn
}

func (l *link) isDown() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.down
}

// cut takes the link down. Every live socket gets a read deadline in
// the past, which unblocks the library's read loop at once without
// closing anything, and moves to held so the close that follows is a
// no-op.
func (l *link) cut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.down = true
	for _, c := range l.live {
		_ = c.SetReadDeadline(time.Now().Add(-time.Second))
	}
	l.held = append(l.held, l.live...)
	l.live = nil
}

// restore brings the link back and closes the sockets held open across
// the outage. Closing here rather than at cut time is what kept the
// broker in the dark for the length of the fault.
func (l *link) restore() {
	l.mu.Lock()
	held := l.held
	l.held, l.down = nil, false
	l.mu.Unlock()
	for _, c := range held {
		_ = c.Close()
	}
}

// hold records a freshly dialled socket as library-owned.
func (l *link) hold(c net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live = append(l.live, c)
}

// release reports whether closing this socket is the caller's to do. A
// socket the outage already took is not: it stays open until restore.
func (l *link) release(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, live := range l.live {
		if live == c {
			l.live = append(l.live[:i], l.live[i+1:]...)
			return true
		}
	}
	return false
}

// severableConn is the net.Conn the MQTT library is given. It fails
// reads and swallows writes while the link is down, and leaves the
// underlying socket open for the length of an outage.
type severableConn struct {
	net.Conn
	link *link
}

func (s *severableConn) Read(b []byte) (int, error) {
	if s.link.isDown() {
		return 0, ErrLinkDown
	}
	n, err := s.Conn.Read(b)
	if err != nil && s.link.isDown() {
		// The read the cut unblocked. Report the cause rather than a
		// deadline the library would log as a broker problem.
		return n, ErrLinkDown
	}
	return n, err
}

// Write reports success and sends nothing. A CPE with a dead uplink
// hands its packets to a route that drops them; the socket does not
// report that, and neither does this.
func (s *severableConn) Write(b []byte) (int, error) {
	if s.link.isDown() {
		return len(b), nil
	}
	return s.Conn.Write(b)
}

func (s *severableConn) Close() error {
	if !s.link.release(s.Conn) {
		return nil
	}
	return s.Conn.Close()
}

// LinkDown takes this agent's uplink away: the live session is cut
// without the broker being told, and every reconnect attempt fails
// until LinkUp. Anything published while the link is down goes nowhere.
func (c *Client) LinkDown() {
	outages.Add(1)
	c.link.cut()
	c.log.Info("usp/mqtt: uplink down", "endpoint_id", c.cfg.EndpointID)
}

// LinkUp restores the uplink. The library's own reconnect loop brings
// the session back, the way a CPE that never stopped trying would;
// Connected reports when it has.
func (c *Client) LinkUp() {
	c.link.restore()
	outages.Add(-1)
	c.log.Info("usp/mqtt: uplink up", "endpoint_id", c.cfg.EndpointID)
}

// Connected reports whether the agent currently holds a broker session.
//
// IsConnectionOpen and not IsConnected: the latter answers "will this
// client deliver a message eventually", which is true throughout a
// reconnect, so a caller waiting for an agent to come back would be
// told it already had. What a caller wants to know here is whether
// there is a session right now.
func (c *Client) Connected() bool {
	return c.client != nil && c.client.IsConnectionOpen()
}
