package generators

import (
	"errors"
	"fmt"
	"math/rand" //nolint:gosec // behavior randomness, not security
	"sync"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// EnumConfig configures an enum-cycle Generator.
type EnumConfig struct {
	Path string

	// Values is the ordered list to cycle through (or pick randomly
	// from). Must be non-empty. Repeat entries to weight a random pool
	// (e.g. ["Up","Up","Up","Up","Down"] -> 80% Up / 20% Down).
	Values []string

	// Mode is "cycle" (default; sequential, wraps) or "random"
	// (uniform pick each tick).
	Mode string
}

// NewEnum validates cfg and returns an enum Generator.
//
// Cycle mode: the Tick reads the leaf's current Raw, finds its index
// in Values, and writes Values[(index+1) % len(Values)]. If the
// current value is not in the list, starts at index 0.
//
// Random mode: Tick writes a uniformly-chosen entry from Values
// (independent of the current value).
func NewEnum(cfg EnumConfig) (Generator, error) {
	if cfg.Path == "" {
		return nil, errors.New("generators.NewEnum: Path is required")
	}
	if len(cfg.Values) == 0 {
		return nil, errors.New("generators.NewEnum: Values must be non-empty")
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "cycle"
	}
	if mode != "cycle" && mode != "random" {
		return nil, fmt.Errorf("generators.NewEnum: Mode %q invalid (want cycle|random)", mode)
	}
	out := cfg
	out.Mode = mode
	return &enumGen{cfg: out}, nil
}

type enumGen struct {
	cfg EnumConfig

	// Cycle position is stored on the generator (rather than derived
	// from the tree value) so duplicate entries in Values still
	// advance, looking up the current value finds the FIRST match
	// and would never move past it.
	mu  sync.Mutex
	idx int // next index to emit; starts at 0
}

func (g *enumGen) Path() string { return g.cfg.Path }

func (g *enumGen) Tick(tree *paramtree.Tree, rng *rand.Rand) (string, error) {
	// Existence check; the write below goes through SetSystem, which
	// validates against the leaf's declared type itself.
	if _, err := tree.Get(g.cfg.Path); err != nil {
		return "", err
	}

	var next string
	switch g.cfg.Mode {
	case "random":
		if rng == nil {
			return "", errors.New("generators.enum: random mode requires non-nil rng")
		}
		next = g.cfg.Values[rng.Intn(len(g.cfg.Values))]
	default: // cycle
		g.mu.Lock()
		next = g.cfg.Values[g.idx]
		g.idx = (g.idx + 1) % len(g.cfg.Values)
		g.mu.Unlock()
	}

	if err := tree.SetSystem(g.cfg.Path, next); err != nil {
		return "", err
	}
	return next, nil
}
