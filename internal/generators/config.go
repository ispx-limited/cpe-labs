package generators

import (
	"fmt"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// FromConfig builds the generator a validated profile or manifest
// entry describes. The profile loader has already checked the target
// leaf's type and the parameter ranges, so the only failure left is a
// kind this build does not know.
func FromConfig(cfg paramtree.GeneratorConfig) (Generator, error) {
	switch cfg.Type {
	case "counter":
		if cfg.Counter == nil {
			return nil, fmt.Errorf("generator %q: counter block missing", cfg.Path)
		}
		return NewCounter(CounterConfig{
			Path:   cfg.Path,
			Min:    cfg.Counter.Min,
			Max:    cfg.Counter.Max,
			Step:   cfg.Counter.Step,
			Jitter: cfg.Counter.Jitter,
		})
	case "drift":
		if cfg.Drift == nil {
			return nil, fmt.Errorf("generator %q: drift block missing", cfg.Path)
		}
		return NewDrift(DriftConfig{
			Path:    cfg.Path,
			Min:     cfg.Drift.Min,
			Max:     cfg.Drift.Max,
			StepMax: cfg.Drift.StepMax,
		})
	case "enum":
		if cfg.Enum == nil {
			return nil, fmt.Errorf("generator %q: enum block missing", cfg.Path)
		}
		return NewEnum(EnumConfig{
			Path:   cfg.Path,
			Values: cfg.Enum.Values,
			Mode:   cfg.Enum.Mode,
		})
	case "uptime":
		return NewTimestamp(TimestampConfig{Path: cfg.Path, Kind: TimestampUptime})
	case "wallclock":
		return NewTimestamp(TimestampConfig{Path: cfg.Path, Kind: TimestampWallclock})
	default:
		return nil, fmt.Errorf("generator %q: type %q unsupported", cfg.Path, cfg.Type)
	}
}

// AddConfig builds and registers one configured generator.
func (r *Runner) AddConfig(cfg paramtree.GeneratorConfig) error {
	gen, err := FromConfig(cfg)
	if err != nil {
		return err
	}
	if err := r.Add(gen, cfg.Interval); err != nil {
		return fmt.Errorf("generator %q: %w", cfg.Path, err)
	}
	return nil
}
