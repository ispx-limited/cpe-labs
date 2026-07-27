package inform

import (
	"fmt"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Builder produces Inform values from a parameter tree, an event list,
// and a retry counter. Read-only after construction; concurrent Build
// calls are safe.
type Builder struct {
	tree           *paramtree.Tree
	deviceIDPaths  DeviceIDPaths
	parameterLists map[string][]string
	clock          func() time.Time
	maxEnvelopes   uint
}

// NewBuilder validates opts and returns a Builder bound to tree.
// Returns cpeerr.KindInvalidArgument if any DeviceIDPath is empty or
// does not resolve to an existing leaf in the tree (caught early so a
// misconfigured profile fails loudly at startup).
//
// All four DeviceIDPaths fields are required: the inform package ships no
// TR-181, TR-098 or vendor-specific defaults, per design principle #3
// (behavior is config, not code). Operators declare which
// paths hold DeviceId fields in their profile's deviceIdPaths block.
func NewBuilder(tree *paramtree.Tree, opts BuilderOptions) (*Builder, error) {
	if tree == nil {
		return nil, cpeerr.Wrap("inform.NewBuilder", cpeerr.KindInvalidArgument,
			fmt.Errorf("tree is nil"))
	}

	paths := opts.DeviceIDPaths
	for _, e := range []struct{ field, path string }{
		{"Manufacturer", paths.Manufacturer},
		{"OUI", paths.OUI},
		{"ProductClass", paths.ProductClass},
		{"SerialNumber", paths.SerialNumber},
	} {
		if e.path == "" {
			return nil, cpeerr.Wrap("inform.NewBuilder", cpeerr.KindInvalidArgument,
				fmt.Errorf("DeviceIDPaths.%s is required (no TR-181 default; "+
					"declare deviceIdPaths in the vendor profile)", e.field))
		}
		if _, err := tree.Get(e.path); err != nil {
			return nil, cpeerr.Wrap("inform.NewBuilder", cpeerr.KindInvalidArgument,
				fmt.Errorf("DeviceIDPaths.%s path %q: %w", e.field, e.path, err))
		}
	}

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Builder{
		tree:           tree,
		deviceIDPaths:  paths,
		parameterLists: opts.ParameterLists,
		clock:          clock,
		maxEnvelopes:   opts.MaxEnvelopes,
	}, nil
}

// Build assembles an Inform value from the current tree state, the
// event list, and the retry counter. Returns
// cpeerr.KindInvalidArgument if events is empty or if a referenced
// parameter-list path resolves to a missing or interior-node target.
func (b *Builder) Build(events []Event, retryCount uint) (*Inform, error) {
	if len(events) == 0 {
		return nil, cpeerr.Wrap("inform.Build", cpeerr.KindInvalidArgument,
			fmt.Errorf("events is empty"))
	}

	deviceID, err := b.readDeviceID()
	if err != nil {
		return nil, err
	}

	params, err := b.collectParameters(events)
	if err != nil {
		return nil, err
	}

	return &Inform{
		DeviceID:     deviceID,
		Events:       append([]Event(nil), events...),
		MaxEnvelopes: maxEnvelopes(b.maxEnvelopes),
		CurrentTime:  b.clock(),
		RetryCount:   retryCount,
		Parameters:   params,
	}, nil
}

func (b *Builder) readDeviceID() (DeviceID, error) {
	read := func(path string) (string, error) {
		v, err := b.tree.Get(path)
		if err != nil {
			return "", cpeerr.Wrap("inform.Build", cpeerr.KindInvalidArgument,
				fmt.Errorf("read %s: %w", path, err))
		}
		return v.Raw, nil
	}
	var d DeviceID
	var err error
	if d.Manufacturer, err = read(b.deviceIDPaths.Manufacturer); err != nil {
		return DeviceID{}, err
	}
	if d.OUI, err = read(b.deviceIDPaths.OUI); err != nil {
		return DeviceID{}, err
	}
	if d.ProductClass, err = read(b.deviceIDPaths.ProductClass); err != nil {
		return DeviceID{}, err
	}
	if d.SerialNumber, err = read(b.deviceIDPaths.SerialNumber); err != nil {
		return DeviceID{}, err
	}
	return d, nil
}

// collectParameters picks the first event whose code is a key in
// b.parameterLists and reads each named path from the tree. Returns
// nil (an empty list) when no event matches.
func (b *Builder) collectParameters(events []Event) ([]Parameter, error) {
	var paths []string
	for _, e := range events {
		if list, ok := b.parameterLists[e.EventCode]; ok {
			paths = list
			break
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}

	out := make([]Parameter, 0, len(paths))
	for _, p := range paths {
		v, err := b.tree.Get(p)
		if err != nil {
			return nil, cpeerr.Wrap("inform.Build", cpeerr.KindInvalidArgument,
				fmt.Errorf("read parameter %s: %w", p, err))
		}
		out = append(out, Parameter{Name: p, Value: v})
	}
	return out, nil
}
