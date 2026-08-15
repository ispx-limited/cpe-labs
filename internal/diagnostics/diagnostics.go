// Package diagnostics runs the TR-069 triggered diagnostics a profile
// declares: an ACS writes a trigger value, the CPE works for a while,
// then moves the state parameter to a terminal value.
//
// The shape matters more than the work. An ACS does not watch a
// neighbour sweep happen; it writes "Requested", polls the state, and
// reads the results once the state changes. A simulator that completed
// instantly would let ACS code pass that has never handled the
// intermediate state, and one that never completed at all would look
// exactly like the vendor firmware bugs this exists to rule out.
package diagnostics

import (
	"context"
	"sync"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Runner watches parameter writes and completes diagnostics.
//
// One Runner serves one CPE's tree. Writes arrive on the same callback
// the value-change notifier uses, so a diagnostic costs nothing until
// an ACS actually asks for one.
type Runner struct {
	tree  *paramtree.Tree
	byGap map[string]paramtree.DiagnosticConfig

	// inFlight guards against a second trigger landing while a run is
	// still going. A real CPE restarts the sweep; what it does not do
	// is run two sweeps at once and interleave their results, which is
	// what an unguarded timer per write would simulate.
	//
	// Keyed by state path and held as a pointer so a cancelled run can
	// tell whether the entry is still its own: it is the superseded
	// run that wakes first, and a blind delete there would clear the
	// entry the run that replaced it is relying on.
	mu       sync.Mutex
	inFlight map[string]*run

	// now and after are injectable so tests do not sleep. Nil means
	// real time.
	after func(time.Duration) <-chan time.Time
}

// New returns a Runner for the given tree and diagnostics. A nil or
// empty list yields a Runner whose OnWrite is a no-op, so callers need
// no conditional wiring.
func New(tree *paramtree.Tree, diags []paramtree.DiagnosticConfig) *Runner {
	byGap := make(map[string]paramtree.DiagnosticConfig, len(diags))
	for _, d := range diags {
		byGap[d.StatePath] = d
	}
	return &Runner{
		tree:     tree,
		byGap:    byGap,
		inFlight: make(map[string]*run),
	}
}

// run is one execution of one diagnostic, identified by its own
// address.
type run struct {
	cancel context.CancelFunc
}

// OnWrite is called after a successful write to path. It starts a run
// when the written value is the diagnostic's trigger, and ignores
// everything else.
//
// Deliberately tolerant of writes it does not recognise: the callback
// is shared with the value-change notifier and sees every set on the
// tree, so anything other than an exact trigger match must be cheap
// and silent.
func (r *Runner) OnWrite(ctx context.Context, path string) {
	diag, ok := r.byGap[path]
	if !ok {
		return
	}
	v, err := r.tree.Get(path)
	if err != nil || v.Raw != diag.Trigger {
		return
	}

	r.mu.Lock()
	if prev, running := r.inFlight[path]; running {
		// Re-triggered mid-run. Abandon the old timer and start again,
		// which is what a CPE asked to rescan does.
		prev.cancel()
	}
	runCtx, cancel := context.WithCancel(ctx)
	this := &run{cancel: cancel}
	r.inFlight[path] = this
	r.mu.Unlock()

	go r.complete(runCtx, this, diag)
}

// complete waits out the run and writes the terminal state.
func (r *Runner) complete(ctx context.Context, this *run, diag paramtree.DiagnosticConfig) {
	defer func() {
		r.mu.Lock()
		if r.inFlight[diag.StatePath] == this {
			delete(r.inFlight, diag.StatePath)
		}
		r.mu.Unlock()
	}()

	after := r.after
	if after == nil {
		after = time.After
	}
	select {
	case <-ctx.Done():
		// Cancelled by a re-trigger or by shutdown. Leaving the state
		// on the trigger value is correct for the first case (the new
		// run owns it now) and harmless for the second, where the
		// process is going away with its tree.
		return
	case <-after(diag.Duration):
	}

	// Count first, then state. An ACS that polls the state and reads
	// the table the moment it changes must not observe Complete with a
	// stale count, which is a real race on real hardware and one an
	// ACS integration will eventually hit.
	if diag.CountPath != "" {
		_ = r.tree.SetSystem(diag.CountPath, itoa(diag.ResultCount))
	}
	_ = r.tree.SetSystem(diag.StatePath, diag.Complete)
}

// itoa avoids importing strconv for one call in a hot-ish path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
