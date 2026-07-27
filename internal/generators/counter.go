package generators

import (
	"errors"
	"fmt"
	"math/rand" //nolint:gosec // behavior randomness, not security
	"strconv"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// bbfUint32Max mirrors the xsd:unsignedInt ceiling enforced by
// internal/paramtree.types. Counter Max above this would always
// fail Tree.Set; failing at construction is friendlier.
const bbfUint32Max = uint64(4294967295)

// CounterConfig configures a counter Generator. See
// paramtree.CounterParams for field semantics, this struct is the
// internal mirror so the package does not need to import paramtree's
// profile schema.
type CounterConfig struct {
	Path   string
	Min    uint64
	Max    uint64
	Step   uint64
	Jitter float64
}

// NewCounter validates cfg and returns a counter Generator.
//
// Validation:
//   - Path non-empty
//   - Step > 0
//   - Min < Max
//   - Max <= 4294967295 (uint32 ceiling)
//   - Jitter in [0.0, 1.0]
//
// Counter math: each Tick reads the leaf's current Raw, advances by
// Step ± Jitter*Step (uniform random), and wraps from Max+1 back to
// Min. If SPV has set the leaf outside [Min, Max], the next Tick
// clamps to Min and proceeds rather than refusing to advance.
func NewCounter(cfg CounterConfig) (Generator, error) {
	if cfg.Path == "" {
		return nil, errors.New("generators.NewCounter: Path is required")
	}
	if cfg.Step == 0 {
		return nil, errors.New("generators.NewCounter: Step must be > 0")
	}
	if cfg.Min >= cfg.Max {
		return nil, fmt.Errorf("generators.NewCounter: Min (%d) must be < Max (%d)", cfg.Min, cfg.Max)
	}
	if cfg.Max > bbfUint32Max {
		return nil, fmt.Errorf("generators.NewCounter: Max %d exceeds xsd:unsignedInt ceiling %d", cfg.Max, bbfUint32Max)
	}
	if cfg.Jitter < 0 || cfg.Jitter > 1 {
		return nil, fmt.Errorf("generators.NewCounter: Jitter must be in [0.0, 1.0], got %g", cfg.Jitter)
	}
	return &counterGen{cfg: cfg}, nil
}

type counterGen struct {
	cfg CounterConfig
}

func (c *counterGen) Path() string { return c.cfg.Path }

func (c *counterGen) Tick(tree *paramtree.Tree, rng *rand.Rand) (string, error) {
	cur, err := tree.Get(c.cfg.Path)
	if err != nil {
		return "", err
	}
	state, err := strconv.ParseUint(cur.Raw, 10, 64)
	if err != nil {
		return "", fmt.Errorf("parse current value %q: %w", cur.Raw, err)
	}
	if state < c.cfg.Min || state > c.cfg.Max {
		// SPV (or fresh-from-profile init) may have set the leaf outside
		// the configured band. Clamp to Min and proceed rather than
		// refusing to advance, operator intent is "make this move."
		state = c.cfg.Min
	}

	inc := c.cfg.Step
	if c.cfg.Jitter > 0 && rng != nil {
		span := uint64(float64(c.cfg.Step) * c.cfg.Jitter)
		if span > 0 {
			//nolint:gosec // behavior randomness, not security
			delta := uint64(rng.Int63n(int64(2*span)+1)) - span
			inc = c.cfg.Step + delta
		}
	}

	next := state + inc
	if next > c.cfg.Max {
		// Wrap from max+1 back to min (real CPE counter behavior at
		// uint32 boundaries).
		over := next - c.cfg.Max - 1
		next = c.cfg.Min + over%(c.cfg.Max-c.cfg.Min+1)
	}
	raw := strconv.FormatUint(next, 10)
	if err := tree.SetSystem(c.cfg.Path, raw); err != nil {
		return "", err
	}
	return raw, nil
}
