package paramtree

import (
	"fmt"
	"strconv"
	"strings"
)

// Bulk Data Collection for the TR-069 side, TR-157 Annex A as it appears
// on a real device rather than as the standard describes it.
//
// This is opt-in per profile, unlike the TR-369 agent's Device.BulkData.
// which every USP agent mounts. On CWMP the capability is a vendor fact:
// a fleet survey answers it, most of the installed base does not have the
// subtree at all, and a simulator that gave every device one would make
// the survey answer a question nobody asked. A profile declares the
// bulkData: block when the hardware it models advertises the capability,
// with the values that hardware actually reports.
//
// The parameter set is the one an ARRIS NVG578LX on firmware 9.5.0
// exposes. Two absences are deliberate and load-bearing:
//
//   - No Alias. TR-106 alias-based addressing is optional on CWMP and
//     this hardware does not implement it, so an ACS cannot mark the rows
//     it owns that way and cannot address a nested Parameter row through
//     the alias it created the parent with. Adding one here would let an
//     ACS pass against the simulator and fail against the device.
//   - No Parameter.{i}.Exclude. That arrived in a later TR-181 revision;
//     the TR-098-rooted spelling predates it.

// BulkDataConfig is the profile's bulkData: block: the capability
// parameters a device advertises, which decide whether an ACS can use
// the mechanism and how narrowly it can ask.
type BulkDataConfig struct {
	// Root is the data model root the subtree hangs off,
	// "InternetGatewayDevice" or "Device".
	Root string
	// Protocols and EncodingTypes are the comma-separated transport and
	// encoding lists, verbatim as the device reports them.
	Protocols     string
	EncodingTypes string
	// MinReportingInterval is the floor, in seconds, on
	// Profile.{i}.ReportingInterval.
	MinReportingInterval int
	// MaxNumberOfProfiles and MaxNumberOfParameterReferences are the two
	// hard limits. -1 is the data model's spelling of unlimited.
	MaxNumberOfProfiles            int
	MaxNumberOfParameterReferences int
	// ParameterWildCardSupported reports whether a Reference may carry a
	// "*" in place of an instance identifier. False does not stop an
	// object path collecting a whole table: the two are separate
	// mechanisms and only the wildcard is gated on this flag.
	ParameterWildCardSupported bool
}

// MountBulkData mounts <root>.BulkData. and its writable Profile table.
// Called at profile load, so every CPE cloned from the template carries
// it.
func MountBulkData(t *Tree, cfg BulkDataConfig) error {
	root := strings.TrimSuffix(cfg.Root, ".")
	if root == "" {
		return fmt.Errorf("bulkData: root is empty")
	}
	base := root + ".BulkData"

	if err := t.Mount(base, NewBranch()); err != nil {
		return err
	}
	for _, p := range []struct {
		name     string
		typ      Type
		value    string
		writable bool
	}{
		{"Enable", TypeBoolean, "0", true},
		{"Status", TypeString, "Disabled", false},
		{"MinReportingInterval", TypeUnsignedInt, strconv.Itoa(cfg.MinReportingInterval), false},
		{"Protocols", TypeString, cfg.Protocols, false},
		{"EncodingTypes", TypeString, cfg.EncodingTypes, false},
		{"ParameterWildCardSupported", TypeBoolean, boolRaw(cfg.ParameterWildCardSupported), false},
		{"MaxNumberOfProfiles", TypeInt, strconv.Itoa(cfg.MaxNumberOfProfiles), false},
		{"MaxNumberOfParameterReferences", TypeInt, strconv.Itoa(cfg.MaxNumberOfParameterReferences), false},
		// Kept in step with the table by AddObject and DeleteObject, the
		// same way a real CPE advertises its table size.
		{"ProfileNumberOfEntries", TypeUnsignedInt, "0", false},
	} {
		if err := t.Mount(base+"."+p.name, NewLeaf(Value{
			Type: p.typ, Raw: p.value, Writable: p.writable,
		})); err != nil {
			return err
		}
	}

	profile, err := bulkDataProfileTemplate()
	if err != nil {
		return err
	}
	if err := t.Mount(base+".Profile", NewBranch()); err != nil {
		return err
	}
	return t.AddTable(base+".Profile", profile)
}

// bulkDataProfileTemplate builds one Profile.{i}. instance. Parameter.
// {i}. and HTTP.RequestURIParameter.{i}. are tables inside the instance,
// so they are attached to the template node rather than mounted by path:
// a path may carry {i} only once, and every instance an ACS adds needs
// its own.
func bulkDataProfileTemplate() (*Node, error) {
	profile := NewBranch()
	for _, p := range []struct {
		name     string
		typ      Type
		value    string
		writable bool
	}{
		{"Enable", TypeBoolean, "0", true},
		{"Name", TypeString, "", true},
		{"NumberOfRetainedFailedReports", TypeInt, "0", true},
		{"Protocol", TypeString, "", true},
		{"EncodingType", TypeString, "", true},
		// 86400 is what the hardware ships: a profile arrives inert at a
		// daily cadence rather than starting to report the moment it is
		// created.
		{"ReportingInterval", TypeUnsignedInt, "86400", true},
		{"TimeReference", TypeDateTime, "0001-01-01T00:00:00Z", true},
		{"ParameterNumberOfEntries", TypeUnsignedInt, "0", false},
		// Vendor status leaf, alongside the standard BulkData.Status.
		{"X_0000C5_Status", TypeString, "Disabled", false},
	} {
		if err := profile.Attach(p.name, NewLeaf(Value{
			Type: p.typ, Raw: p.value, Writable: p.writable,
		})); err != nil {
			return nil, err
		}
	}

	jsonEncoding := NewBranch()
	for _, p := range []struct{ name, value string }{
		{"ReportFormat", "ObjectHierarchy"},
		{"ReportTimestamp", "Unix-Epoch"},
	} {
		if err := jsonEncoding.Attach(p.name, NewLeaf(Value{
			Type: TypeString, Raw: p.value, Writable: true,
		})); err != nil {
			return nil, err
		}
	}
	if err := profile.Attach("JSONEncoding", jsonEncoding); err != nil {
		return nil, err
	}

	http, err := bulkDataHTTPTemplate()
	if err != nil {
		return nil, err
	}
	if err := profile.Attach("HTTP", http); err != nil {
		return nil, err
	}

	nameRef := NewBranch()
	for _, name := range []string{"Name", "Reference"} {
		if err := nameRef.Attach(name, NewLeaf(Value{Type: TypeString, Writable: true})); err != nil {
			return nil, err
		}
	}
	if err := profile.Attach("Parameter", NewTable(nameRef)); err != nil {
		return nil, err
	}
	return profile, nil
}

// bulkDataHTTPTemplate is Profile.{i}.HTTP., the delivery half: where the
// report goes and the credential it carries.
func bulkDataHTTPTemplate() (*Node, error) {
	http := NewBranch()
	for _, p := range []struct {
		name     string
		typ      Type
		value    string
		writable bool
	}{
		{"URL", TypeString, "", true},
		{"Username", TypeString, "", true},
		{"Password", TypeString, "", true},
		{"CompressionsSupported", TypeString, "GZIP,Compress,Deflate", false},
		{"Compression", TypeString, "None", true},
		{"MethodsSupported", TypeString, "POST,PUT", false},
		{"Method", TypeString, "POST", true},
		{"UseDateHeader", TypeBoolean, "1", true},
		{"RetryEnable", TypeBoolean, "1", true},
		{"RetryMinimumWaitInterval", TypeUnsignedInt, "5", true},
		{"RetryIntervalMultiplier", TypeUnsignedInt, "2000", true},
		{"RequestURIParameterNumberOfEntries", TypeUnsignedInt, "0", false},
	} {
		if err := http.Attach(p.name, NewLeaf(Value{
			Type: p.typ, Raw: p.value, Writable: p.writable,
		})); err != nil {
			return nil, err
		}
	}

	// RequestURIParameter rows put device identity in the query string.
	// An ACS may read them as a correlation hint; they are not proof of
	// identity, which rests on the per-device credential above.
	uriParam := NewBranch()
	for _, name := range []string{"Name", "Reference"} {
		if err := uriParam.Attach(name, NewLeaf(Value{Type: TypeString, Writable: true})); err != nil {
			return nil, err
		}
	}
	if err := http.Attach("RequestURIParameter", NewTable(uriParam)); err != nil {
		return nil, err
	}
	return http, nil
}

func boolRaw(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// validateBulkData turns a profile's bulkData: block into a config,
// rejecting the values that would model a device that cannot exist.
func validateBulkData(source string, raw *rawBulkData) (BulkDataConfig, error) {
	cfg := BulkDataConfig{
		Root:                           raw.Root,
		Protocols:                      raw.Protocols,
		EncodingTypes:                  raw.EncodingTypes,
		MinReportingInterval:           raw.MinReportingInterval,
		MaxNumberOfProfiles:            raw.MaxNumberOfProfiles,
		MaxNumberOfParameterReferences: raw.MaxNumberOfParameterReferences,
		ParameterWildCardSupported:     raw.ParameterWildCardSupported,
	}
	switch cfg.Root {
	case "InternetGatewayDevice", "Device":
	case "":
		return cfg, fmt.Errorf("%s: bulkData.root is required", source)
	default:
		return cfg, fmt.Errorf("%s: bulkData.root %q is not a data model root", source, cfg.Root)
	}
	if cfg.Protocols == "" {
		return cfg, fmt.Errorf("%s: bulkData.protocols is required", source)
	}
	if cfg.EncodingTypes == "" {
		return cfg, fmt.Errorf("%s: bulkData.encodingTypes is required", source)
	}
	if cfg.MinReportingInterval < 1 {
		return cfg, fmt.Errorf("%s: bulkData.minReportingInterval must be at least 1", source)
	}
	// -1 is unlimited; 0 would advertise a device that can hold no
	// profiles at all, which is the same as not having the capability.
	if cfg.MaxNumberOfProfiles == 0 || cfg.MaxNumberOfProfiles < -1 {
		return cfg, fmt.Errorf("%s: bulkData.maxNumberOfProfiles must be -1 or a positive count", source)
	}
	if cfg.MaxNumberOfParameterReferences == 0 || cfg.MaxNumberOfParameterReferences < -1 {
		return cfg, fmt.Errorf("%s: bulkData.maxNumberOfParameterReferences must be -1 or a positive count", source)
	}
	return cfg, nil
}
