package generators

import (
	"errors"
	"math/rand" //nolint:gosec // required by Generator interface
	"strconv"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// TimestampKind discriminates the timestamp generator's behavior.
type TimestampKind int

const (
	// TimestampUptime writes monotonic seconds since the generator
	// was constructed, as xsd:unsignedInt. Models Device.DeviceInfo.UpTime
	// and similar elapsed-time leaves.
	TimestampUptime TimestampKind = iota

	// TimestampWallclock writes the current UTC time formatted as
	// RFC 3339 with second precision, as xsd:dateTime. Models
	// Device.Time.CurrentLocalTime and any leaf the ACS expects to
	// reflect "now".
	TimestampWallclock
)

// TimestampConfig configures a timestamp Generator.
type TimestampConfig struct {
	Path string
	Kind TimestampKind
}

// NewTimestamp validates cfg and returns a Generator.
func NewTimestamp(cfg TimestampConfig) (Generator, error) {
	if cfg.Path == "" {
		return nil, errors.New("generators.NewTimestamp: Path is required")
	}
	if cfg.Kind != TimestampUptime && cfg.Kind != TimestampWallclock {
		return nil, errors.New("generators.NewTimestamp: Kind must be TimestampUptime or TimestampWallclock")
	}
	return &timestampGen{cfg: cfg, start: time.Now()}, nil
}

type timestampGen struct {
	cfg   TimestampConfig
	start time.Time
}

func (g *timestampGen) Path() string { return g.cfg.Path }

func (g *timestampGen) Tick(tree *paramtree.Tree, _ *rand.Rand) (string, error) {
	// Existence check; the write below goes through SetSystem, which
	// validates against the leaf's declared type itself.
	if _, err := tree.Get(g.cfg.Path); err != nil {
		return "", err
	}

	var raw string
	switch g.cfg.Kind {
	case TimestampUptime:
		secs := uint64(time.Since(g.start).Seconds())
		raw = strconv.FormatUint(secs, 10)
	case TimestampWallclock:
		raw = time.Now().UTC().Format(time.RFC3339)
	default:
		return "", errors.New("generators.timestamp: unknown kind")
	}

	if err := tree.SetSystem(g.cfg.Path, raw); err != nil {
		return "", err
	}
	return raw, nil
}
