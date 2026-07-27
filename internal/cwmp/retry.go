package cwmp

import (
	"math/rand"
	"sync"
	"time"
)

// TR-069 3.2.1.1 session retry policy factory defaults: minimum wait
// interval m (ManagementServer.CWMPRetryMinimumWaitInterval) and
// interval multiplier k/1000 (ManagementServer.CWMPRetryIntervalMultiplier,
// k=2000). These are the spec's hard-coded protocol defaults, not
// vendor knowledge; a profile-driven override can layer on later
// without changing the curve evaluation.
const (
	retryMinimumWait = 5 * time.Second
	retryMultiplier  = 2.0
	// retryAttemptCap is where Table 3 flattens: beginning with the
	// tenth post-reboot retry attempt the CPE keeps choosing from the
	// fixed maximum range (2560..5120s with factory defaults).
	retryAttemptCap = 10
)

// RetryState tracks one CPE's session retry count and computes the
// TR-069 Table 3 wait band for each attempt. Goroutine-safe.
//
// The count is the number of consecutive failed sessions, which is
// exactly the RetryCount value the next Inform must carry (3.2.1.1:
// "Regardless of the reason a previous session failed or the condition
// prompting session retry, the CPE MUST communicate to the ACS the
// session retry count").
type RetryState struct {
	mu    sync.Mutex
	count uint
	rng   *rand.Rand
}

// NewRetryState returns a RetryState drawing wait intervals from rng.
// rng should be the per-CPE seeded RNG so retry timing is reproducible;
// a nil rng degrades to the deterministic band minimum (test-friendly,
// mirrors the scheduler's jitterPct==0 behavior).
func NewRetryState(rng *rand.Rand) *RetryState {
	return &RetryState{rng: rng}
}

// Count returns the current session retry count: 0 when the previous
// session succeeded, n after n consecutive failures.
func (r *RetryState) Count() uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// OnFailure records a failed session and returns the new retry count
// together with the Table 3 wait before the retry attempt: uniform in
// [m*2^(n-1), m*2^n) seconds for attempt n, flattening at attempt 10.
// With factory defaults that is 5-10s for the first retry, doubling
// per attempt, capped at 2560-5120s.
func (r *RetryState) OnFailure() (count uint, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
	attempt := r.count
	if attempt > retryAttemptCap {
		attempt = retryAttemptCap
	}
	minWait := retryMinimumWait
	for i := uint(1); i < attempt; i++ {
		minWait = time.Duration(float64(minWait) * retryMultiplier)
	}
	maxWait := time.Duration(float64(minWait) * retryMultiplier)
	wait = minWait
	if r.rng != nil {
		wait += time.Duration(r.rng.Int63n(int64(maxWait - minWait)))
	}
	return r.count, wait
}

// Reset clears the retry count. Called after a successfully terminated
// session (3.2.1.1: the CPE MUST reset the session retry count to zero)
// and on reboot (retrying after an intervening reboot restarts the wait
// intervals as though it were the first retry attempt).
func (r *RetryState) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count = 0
}
