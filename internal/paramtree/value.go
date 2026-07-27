package paramtree

// Type is a BBF primitive type identifier. The set of constants is
// declared here for callers; per-Type validation logic lands
// and is wired into Set then. In this package, Type is treated as an
// opaque string identifier used only for the cross-Set type-stability
// check (a Set cannot change a leaf's Type).
type Type string

// BBF primitive type identifiers used across TR-069 / TR-181 schemas.
const (
	TypeString      Type = "xsd:string"
	TypeInt         Type = "xsd:int"
	TypeUnsignedInt Type = "xsd:unsignedInt"
	TypeBoolean     Type = "xsd:boolean"
	TypeDateTime    Type = "xsd:dateTime"
	TypeBase64      Type = "xsd:base64"
)

// Value is the leaf datum stored at a parameter path.
//
// Raw is the canonical string form of the value (the form CWMP / USP
// transports emit on the wire). Type names the BBF primitive Raw
// decodes as. Writable controls whether Set may overwrite this value.
type Value struct {
	Type     Type
	Raw      string
	Writable bool
}

// Attributes are the per-parameter CWMP metadata SetParameterAttributes
// mutates and GetParameterAttributes reads. Storage is independent of
// Value: writes to the tree (Tree.Set, Tree.SetBatch) do not touch
// Attributes, and Tree.SetAttributes does not touch the leaf Value.
//
// The zero value (Notification=0, AccessList=nil) is the BBF default
// and is what GetAttributes returns for a leaf that no caller has
// explicitly written attributes to.
type Attributes struct {
	// Notification is the per-parameter notification mode.
	//   0 = off, the CPE need not inform the ACS of changes
	//   1 = passive, include changes in the next Inform's ParameterList
	//   2 = active, the CPE initiates a session on change
	Notification int

	// AccessList enumerates entities allowed to write the parameter
	// other than the ACS. The BBF default is one entry, "Subscriber".
	// nil and empty are distinct: nil renders as the BBF default,
	// empty renders as "no LAN-side write access".
	AccessList []string
}
