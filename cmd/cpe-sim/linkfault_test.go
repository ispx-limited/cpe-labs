package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// fakeLink records what the fault did to the uplink and in what order.
type fakeLink struct {
	mu        sync.Mutex
	events    []string
	connected bool
}

func (f *fakeLink) LinkDown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "down")
	f.connected = false
}

func (f *fakeLink) LinkUp() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "up")
	f.connected = true
}

func (f *fakeLink) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeLink) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// fakeAgent records the reports the CPE made when it got back.
type fakeAgent struct {
	mu       sync.Mutex
	reported [][2]string
	boots    []string
	// link is read at report time, and sent records what it said, so a
	// test can assert the agent was only asked to speak once it had
	// somewhere to speak to.
	link *fakeLink
	sent []bool
}

func (f *fakeAgent) ReportValueChange(path, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reported = append(f.reported, [2]string{path, value})
	f.sent = append(f.sent, f.link.Connected())
}

func (f *fakeAgent) Boot(cause string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boots = append(f.boots, cause)
	return nil
}

func linkFaultStack(t *testing.T, withLastChange bool) (*cpeStack, *fakeLink, *fakeAgent) {
	t.Helper()
	tree := paramtree.New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("mount: %v", err)
		}
	}
	must(tree.Mount("Device.IP.Interface.1.Status",
		paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeString, Raw: "Up"})))
	if withLastChange {
		must(tree.Mount("Device.IP.Interface.1.LastChange",
			paramtree.NewLeaf(paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "900"})))
	}
	link := &fakeLink{connected: true}
	agent := &fakeAgent{link: link}
	return &cpeStack{id: "cpe-1", instance: 1, tree: tree, uspLink: link, uspAgent: agent}, link, agent
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func linkFaultConfig() paramtree.LinkFaultConfig {
	return paramtree.LinkFaultConfig{
		Interface: "Device.IP.Interface.1.",
		// Short enough to keep the test quick; the sequence does not
		// care how long the window is, only that it happens inside it.
		Duration: 20 * time.Millisecond,
	}
}

func TestLinkFaultTakesTheLinkDownBeforeTheInterface(t *testing.T) {
	st, link, agent := linkFaultStack(t, true)
	cfg := linkFaultConfig()

	// The whole point of the ordering: every write that describes the
	// outage happens while the uplink is already gone, so none of it
	// can reach a controller. Recording the link state alongside each
	// change is how that is checked.
	type change struct {
		path, value string
		linkUp      bool
	}
	var mu sync.Mutex
	var changes []change
	cancel := st.tree.Observe(func(c paramtree.Change) {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, change{c.Path, c.New.Raw, link.Connected()})
	})
	defer cancel()

	runLinkFault(context.Background(), st, cfg, quietLogger())
	cancel()

	mu.Lock()
	defer mu.Unlock()
	var sawDown, sawReset bool
	for _, c := range changes {
		switch {
		case c.path == "Device.IP.Interface.1.Status" && c.value == "Down":
			sawDown = true
			if c.linkUp {
				t.Error("the interface went down while the uplink was still up; the outage would have been reportable")
			}
		case c.path == "Device.IP.Interface.1.LastChange" && c.value == "0" && !sawReset:
			sawReset = true
			if c.linkUp {
				t.Error("LastChange reset while the uplink was still up")
			}
		}
	}
	if !sawDown {
		t.Fatal("the interface never went Down")
	}
	if !sawReset {
		t.Error("LastChange was not reset when the interface changed state")
	}
	if got := link.log(); len(got) != 2 || got[0] != "down" || got[1] != "up" {
		t.Errorf("uplink events = %v, want one down then one up", got)
	}

	// Back up, with the pre-fault state restored rather than a guess.
	if v, err := st.tree.Get("Device.IP.Interface.1.Status"); err != nil || v.Raw != "Up" {
		t.Errorf("Status after the fault = %q (%v), want Up", v.Raw, err)
	}
	if len(agent.boots) != 0 {
		t.Errorf("boots = %v; a cut uplink does not restart a router", agent.boots)
	}
}

func TestLinkFaultReportsTheOutageOnlyOnceBack(t *testing.T) {
	st, _, agent := linkFaultStack(t, true)
	runLinkFault(context.Background(), st, linkFaultConfig(), quietLogger())

	// Down, then how long it lasted, then the reset that says the
	// interface has just changed state again. The order is the content:
	// a controller reading these backwards is told the WAN is down.
	if len(agent.reported) != 3 {
		t.Fatalf("reported %v, want the Down transition, its duration and the reset", agent.reported)
	}
	if agent.reported[0] != [2]string{"Device.IP.Interface.1.Status", "Down"} {
		t.Errorf("first report = %v, want the Down the agent could not send at the time", agent.reported[0])
	}
	if agent.reported[1][0] != "Device.IP.Interface.1.LastChange" {
		t.Errorf("second report = %v, want how long the interface had been down", agent.reported[1])
	}
	if agent.reported[2] != [2]string{"Device.IP.Interface.1.LastChange", "0"} {
		t.Errorf("last report = %v, want LastChange reset; leaving the duration as the current "+
			"value tells an operator the interface has been up for the length of the outage",
			agent.reported[2])
	}
	for i, sent := range agent.sent {
		if !sent {
			t.Errorf("report %d was made while the uplink was still down, so it went nowhere", i)
		}
	}
}

func TestLinkFaultWithoutLastChangeReportsStatusOnly(t *testing.T) {
	st, _, agent := linkFaultStack(t, false)
	runLinkFault(context.Background(), st, linkFaultConfig(), quietLogger())

	if len(agent.reported) != 1 {
		t.Fatalf("reported %v, want only the Status transition on a profile with no LastChange", agent.reported)
	}
	if agent.reported[0][1] != "Down" {
		t.Errorf("reported %v, want the Down transition", agent.reported[0])
	}
	if agent.reported[0][0] != "Device.IP.Interface.1.Status" {
		t.Errorf("reported %v, want the Status transition", agent.reported[0])
	}
}

func TestLinkFaultRebootAnnouncesOne(t *testing.T) {
	st, _, agent := linkFaultStack(t, true)
	cfg := linkFaultConfig()
	cfg.Reboot = true
	runLinkFault(context.Background(), st, cfg, quietLogger())

	if len(agent.boots) != 1 {
		t.Fatalf("boots = %v, want exactly one when the profile asked for a reboot", agent.boots)
	}
}

func TestLinkFaultIgnoresATriggerWhileOneIsInFlight(t *testing.T) {
	st, link, _ := linkFaultStack(t, true)
	cfg := linkFaultConfig()
	cfg.Duration = 300 * time.Millisecond

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runLinkFault(context.Background(), st, cfg, quietLogger())
	}()

	// Second trigger while the first outage is still running. Stacking
	// them would take the device dark for a multiple of the configured
	// window, which is not the outage anyone asked for.
	time.Sleep(50 * time.Millisecond)
	runLinkFault(context.Background(), st, cfg, quietLogger())
	wg.Wait()

	if got := link.log(); len(got) != 2 {
		t.Errorf("uplink events = %v, want one outage from two triggers", got)
	}
}

func TestLinkFaultRestoresTheStateItFound(t *testing.T) {
	st, _, _ := linkFaultStack(t, true)
	// A WAN that was Dormant before the outage is Dormant after it. An
	// outage is not a repair.
	if err := st.tree.SetSystem("Device.IP.Interface.1.Status", "Dormant"); err != nil {
		t.Fatalf("set: %v", err)
	}
	runLinkFault(context.Background(), st, linkFaultConfig(), quietLogger())

	v, err := st.tree.Get("Device.IP.Interface.1.Status")
	if err != nil || v.Raw != "Dormant" {
		t.Errorf("Status after the fault = %q (%v), want the Dormant it started at", v.Raw, err)
	}
}

func TestTriggerLinkFaultsHonoursTheBand(t *testing.T) {
	var stacks []*cpeStack
	var links []*fakeLink
	for i := 1; i <= 4; i++ {
		st, link, _ := linkFaultStack(t, true)
		st.instance = i
		stacks = append(stacks, st)
		links = append(links, link)
	}
	cfg := linkFaultConfig()
	cfg.From, cfg.To = 2, 3

	triggerLinkFaults(context.Background(), stacks, cfg, quietLogger())
	// The faults run on their own goroutines; the window is short, so
	// this is comfortably longer than one.
	time.Sleep(300 * time.Millisecond)

	for i, link := range links {
		instance := i + 1
		darkened := len(link.log()) > 0
		want := instance == 2 || instance == 3
		if darkened != want {
			t.Errorf("instance %d darkened = %v, want %v; the band is what gives an outage a locus",
				instance, darkened, want)
		}
	}
}
