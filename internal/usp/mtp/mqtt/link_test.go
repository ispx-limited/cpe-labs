package mqtt

import (
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// pipeConn is one end of a net.Pipe, which gives a Read that blocks the
// way a real socket's does and a deadline that unblocks it.
func pipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func TestSeveredReadFailsAndSocketStaysOpen(t *testing.T) {
	l := &link{}
	agentSide, brokerSide := pipePair(t)
	l.hold(agentSide)
	conn := &severableConn{Conn: agentSide, link: l}

	// A read in flight when the link is cut, which is where a real
	// agent is: parked waiting for the broker to say something.
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := conn.Read(buf)
		readErr <- err
	}()

	// Let the read block before cutting, so the deadline is what
	// unblocks it rather than a race with the goroutine starting.
	time.Sleep(20 * time.Millisecond)
	l.cut()

	select {
	case err := <-readErr:
		if !errors.Is(err, ErrLinkDown) {
			t.Fatalf("read error = %v, want ErrLinkDown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a severed read never returned; the broker would wait forever and so would the agent")
	}

	// The library's response to a lost connection is to close it. That
	// close must not reach the socket: a FIN is exactly the notice a
	// failed uplink cannot give, and the broker has to time the session
	// out on its own instead.
	if err := conn.Close(); err != nil {
		t.Fatalf("close during outage: %v", err)
	}
	if err := brokerSide.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 1)
	_, err := brokerSide.Read(buf)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("broker side read = %v, want a timeout; the socket was closed and the broker was told", err)
	}
}

func TestWriteDuringOutageGoesNowhere(t *testing.T) {
	l := &link{}
	agentSide, brokerSide := pipePair(t)
	l.hold(agentSide)
	conn := &severableConn{Conn: agentSide, link: l}
	l.cut()

	// A publish made while the uplink is down reports success and is
	// never delivered, which is what handing a packet to a dead route
	// looks like from inside the CPE.
	n, err := conn.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write during outage = %d, %v; want it swallowed", n, err)
	}
	if err := brokerSide.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := brokerSide.Read(buf); err == nil {
		t.Fatalf("the broker received %q from an agent with no uplink", buf)
	}
}

func TestRestoreClosesHeldSockets(t *testing.T) {
	l := &link{}
	agentSide, brokerSide := pipePair(t)
	l.hold(agentSide)
	conn := &severableConn{Conn: agentSide, link: l}

	// The deadline goes on before anything is closed: a closed pipe
	// refuses to take one, and the read below has to be able to fail
	// with a timeout if the socket is still open.
	if err := brokerSide.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	l.cut()
	_ = conn.Close() // the library giving up, held open
	l.restore()

	// Now the socket is really gone, and the broker's side sees it.
	buf := make([]byte, 1)
	_, err := brokerSide.Read(buf)
	if err == nil {
		t.Fatal("held socket was not closed when the link came back")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("held socket outlived the outage; sockets leak one per fault per CPE")
	}
	if l.isDown() {
		t.Error("link still reports down after restore")
	}
}

func TestNormalCloseReachesTheSocket(t *testing.T) {
	l := &link{}
	agentSide, brokerSide := pipePair(t)
	l.hold(agentSide)
	conn := &severableConn{Conn: agentSide, link: l}

	if err := brokerSide.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// No fault in play: a close is an ordinary close, and the far end
	// learns of it at once. This is the path every disconnect that is
	// not an outage takes.
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	buf := make([]byte, 1)
	_, err := brokerSide.Read(buf)
	if err == nil {
		t.Fatal("an ordinary close did not reach the socket")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("an ordinary close left the socket open; only an outage may do that")
	}
}

func TestDialRefusedWhileDown(t *testing.T) {
	c, err := New(Config{BrokerAddr: "127.0.0.1:1", EndpointID: "os::000000-test"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	c.LinkDown()
	// The address is deliberately unreachable, so a link that is up
	// would fail too. What is asserted is the reason: refused by the
	// simulated uplink, before any dial, which is what keeps an agent's
	// reconnect loop spinning for the length of an outage.
	uri, err := url.Parse("tcp://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := c.openConnection(uri, *paho.NewClientOptions()); !errors.Is(err, ErrLinkDown) {
		t.Fatalf("dial during outage = %v, want ErrLinkDown", err)
	}
	c.LinkUp()
	if c.link.isDown() {
		t.Error("link still down after LinkUp")
	}
}
