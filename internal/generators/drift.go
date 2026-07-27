package generators

import (
	"errors"
	"fmt"
	"math/rand" //nolint:gosec // behavior randomness, not security
	"strconv"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// DriftConfig configures a drift Generator.
type DriftConfig struct {
	Path    string
	Min     int64
	Max     int64
	StepMax int64
}

// NewDrift validates cfg and returns a drift Generator.
//
// Behavior: each Tick reads the leaf's current Raw, picks a uniform
// delta in [-StepMax, +StepMax] from the per-CPE rng, adds it, and
// clamps the result to [Min, Max]. Models gauges (RSSI, CPU%, temp).
//
// If the leaf is outside [Min, Max] (e.g., set there by SPV), the
// next Tick clamps to the nearest bound and proceeds.
func NewDrift(cfg DriftConfig) (Generator, error) {
	if cfg.Path == "" {
		return nil, errors.New("generators.NewDrift: Path is required")
	}
	if cfg.Min >= cfg.Max {
		return nil, fmt.Errorf("generators.NewDrift: Min (%d) must be < Max (%d)", cfg.Min, cfg.Max)
	}
	if cfg.StepMax <= 0 {
		return nil, fmt.Errorf("generators.NewDrift: StepMax must be > 0, got %d", cfg.StepMax)
	}
	return &driftGen{cfg: cfg}, nil
}

type driftGen struct {
	cfg DriftConfig
}

func (g *driftGen) Path() string { return g.cfg.Path }

func (g *driftGen) Tick(tree *paramtree.Tree, rng *rand.Rand) (string, error) {
	cur, err := tree.Get(g.cfg.Path)
	if err != nil {
		return "", err
	}
	state, err := strconv.ParseInt(cur.Raw, 10, 64)
	if err != nil {
		// SPV may have written something unparseable; reset to mid-band.
		state = (g.cfg.Min + g.cfg.Max) / 2
	}
	if state < g.cfg.Min {
		state = g.cfg.Min
	} else if state > g.cfg.Max {
		state = g.cfg.Max
	}

	var delta int64
	if rng != nil {
		// Uniform in [-StepMax, +StepMax].
		span := 2*g.cfg.StepMax + 1
		delta = rng.Int63n(span) - g.cfg.StepMax
	}
	next := state + delta
	if next < g.cfg.Min {
		next = g.cfg.Min
	} else if next > g.cfg.Max {
		next = g.cfg.Max
	}
	raw := strconv.FormatInt(next, 10)
	if err := tree.SetSystem(g.cfg.Path, raw); err != nil {
		return "", err
	}
	return raw, nil
}
