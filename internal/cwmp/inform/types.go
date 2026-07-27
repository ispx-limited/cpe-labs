// Package inform builds CWMP Inform message bodies from parameter-tree
// state, an event list, and a per-event parameter selection.
//
// Builder.Build reads the tree and produces an *Inform value; Render
// writes the cwmp:Inform body content (everything inside the method
// element, exclusive of framing) to an io.Writer. The SOAP framing
// comes from internal/cwmp/soap.
package inform

import (
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Standard CWMP event-code identifiers from TR-069 Annex A.
const (
	EventBootstrap            = "0 BOOTSTRAP"
	EventBoot                 = "1 BOOT"
	EventPeriodic             = "2 PERIODIC"
	EventScheduled            = "3 SCHEDULED"
	EventValueChange          = "4 VALUE CHANGE"
	EventKicked               = "5 KICKED"
	EventConnectionRequest    = "6 CONNECTION REQUEST"
	EventTransferComplete     = "7 TRANSFER COMPLETE"
	EventDiagnostics          = "8 DIAGNOSTICS COMPLETE"
	EventRequestDownload      = "9 REQUEST DOWNLOAD"
	EventAutonomousTransfer   = "10 AUTONOMOUS TRANSFER COMPLETE"
	EventDUStateChange        = "11 DU STATE CHANGE COMPLETE"
	EventMethodReboot         = "M Reboot"
	EventMethodScheduleInform = "M ScheduleInform"
	EventMethodDownload       = "M Download"
	EventMethodUpload         = "M Upload"
)

// Event is one entry in the cwmp:Inform Event array. CommandKey is
// non-empty only for "M *" events that the ACS triggered with a
// specific command key.
type Event struct {
	EventCode  string
	CommandKey string
}

// DeviceID maps to the DeviceId block in the Inform.
type DeviceID struct {
	Manufacturer string
	OUI          string
	ProductClass string
	SerialNumber string
}

// Parameter is one entry in the ParameterList.
type Parameter struct {
	Name  string
	Value paramtree.Value
}

// Inform is the data the builder produces and the renderer consumes.
type Inform struct {
	DeviceID     DeviceID
	Events       []Event
	MaxEnvelopes uint
	CurrentTime  time.Time
	RetryCount   uint
	Parameters   []Parameter
}

// DeviceIDPaths names the parameter paths the builder reads to
// populate the DeviceId block. All four fields are required; there
// are no TR-181 defaults in this package (design principle #3, no
// vendor / protocol model knowledge in core code). Operators declare
// the right paths in the profile's deviceIdPaths block: TR-181 uses
// Device.DeviceInfo.*, TR-098 uses InternetGatewayDevice.DeviceInfo.*,
// vendor-quirky layouts use whatever they need.
type DeviceIDPaths struct {
	Manufacturer string
	OUI          string
	ProductClass string
	SerialNumber string
}

// BuilderOptions configures the Builder.
type BuilderOptions struct {
	// DeviceIDPaths names the parameter paths the builder reads to
	// populate the DeviceId block. All four fields are required; the
	// inform package ships no defaults (design principle #3).
	DeviceIDPaths DeviceIDPaths

	// ParameterLists maps an event code to the list of parameter paths
	// the Inform should report when that event fires. Build walks
	// Events in order and uses the first event whose code is a key in
	// this map.
	ParameterLists map[string][]string

	// Clock returns the wall-clock time used for cwmp:CurrentTime.
	// Zero value uses time.Now.
	Clock func() time.Time

	// MaxEnvelopes is the cwmp:MaxEnvelopes value. Zero value uses 1.
	MaxEnvelopes uint
}
