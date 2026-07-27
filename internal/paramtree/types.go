package paramtree

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Validate reports whether raw is a valid representation of typ.
// Returns nil for valid input, *cpeerr.Error with KindInvalidArgument
// for invalid input or unknown typ.
func Validate(typ Type, raw string) error {
	fns, ok := typeFns[typ]
	if !ok {
		return cpeerr.Wrap("paramtree.Validate", cpeerr.KindInvalidArgument,
			fmt.Errorf("unknown type %q", typ))
	}
	if err := fns.validate(raw); err != nil {
		return cpeerr.Wrap("paramtree.Validate", cpeerr.KindInvalidArgument,
			fmt.Errorf("invalid %s value %q: %w", typ, raw, err))
	}
	return nil
}

// Marshal returns the canonical wire form of raw for typ. The
// canonical form is what SOAP <Value> elements and USP typed-value
// records emit. For most types the canonical form equals raw when raw
// is well-formed; xsd:boolean and xsd:dateTime perform light
// normalization.
//
// Marshal returns the same error shape as Validate when raw fails to
// parse.
func Marshal(typ Type, raw string) (string, error) {
	fns, ok := typeFns[typ]
	if !ok {
		return "", cpeerr.Wrap("paramtree.Marshal", cpeerr.KindInvalidArgument,
			fmt.Errorf("unknown type %q", typ))
	}
	canonical, err := fns.marshal(raw)
	if err != nil {
		return "", cpeerr.Wrap("paramtree.Marshal", cpeerr.KindInvalidArgument,
			fmt.Errorf("cannot marshal %s value %q: %w", typ, raw, err))
	}
	return canonical, nil
}

type validator func(raw string) error
type marshaler func(raw string) (string, error)

var typeFns = map[Type]struct {
	validate validator
	marshal  marshaler
}{
	TypeString:      {validateString, marshalIdentity},
	TypeInt:         {validateInt32, marshalIdentity},
	TypeUnsignedInt: {validateUint32, marshalIdentity},
	TypeBoolean:     {validateBoolean, marshalBoolean},
	TypeDateTime:    {validateDateTime, marshalDateTime},
	TypeBase64:      {validateBase64, marshalIdentity},
}

// marshalIdentity returns raw unchanged. Most types canonicalize
// trivially: the bytes the caller stored are already the wire form.
func marshalIdentity(raw string) (string, error) {
	return raw, nil
}

func validateString(raw string) error {
	if !utf8.ValidString(raw) {
		return fmt.Errorf("not valid UTF-8")
	}
	return nil
}

func validateInt32(raw string) error {
	if _, err := strconv.ParseInt(raw, 10, 32); err != nil {
		return err
	}
	return nil
}

func validateUint32(raw string) error {
	if _, err := strconv.ParseUint(raw, 10, 32); err != nil {
		return err
	}
	return nil
}

// validateBoolean accepts the four BBF/XSD lexical forms: 0, 1, true,
// false. Marshal canonicalizes to "true" or "false".
func validateBoolean(raw string) error {
	switch raw {
	case "0", "1", "true", "false":
		return nil
	}
	return fmt.Errorf(`want one of "0", "1", "true", "false"`)
}

func marshalBoolean(raw string) (string, error) {
	switch raw {
	case "1", "true":
		return "true", nil
	case "0", "false":
		return "false", nil
	}
	return "", fmt.Errorf(`not a boolean lexical form`)
}

// validateDateTime accepts RFC 3339 with a time-zone designator
// (Z or ±HH:MM). XSD allows naive datetimes; we tighten this because
// real ACS interoperability requires an explicit TZ.
func validateDateTime(raw string) error {
	if !hasTimezone(raw) {
		return fmt.Errorf("missing time-zone designator (Z or ±HH:MM)")
	}
	if _, err := time.Parse(time.RFC3339Nano, raw); err != nil {
		return err
	}
	return nil
}

func marshalDateTime(raw string) (string, error) {
	if !hasTimezone(raw) {
		return "", fmt.Errorf("missing time-zone designator")
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// hasTimezone reports whether raw ends with a recognized RFC 3339
// time-zone designator: "Z", or "±HH:MM" at the tail of the string.
func hasTimezone(raw string) bool {
	if strings.HasSuffix(raw, "Z") {
		return true
	}
	// Look for ±HH:MM at the end (length 6: e.g. "+00:00", "-08:00").
	if len(raw) < 6 {
		return false
	}
	tail := raw[len(raw)-6:]
	if (tail[0] != '+' && tail[0] != '-') || tail[3] != ':' {
		return false
	}
	for _, idx := range []int{1, 2, 4, 5} {
		if tail[idx] < '0' || tail[idx] > '9' {
			return false
		}
	}
	return true
}

func validateBase64(raw string) error {
	if _, err := base64.StdEncoding.DecodeString(raw); err != nil {
		return err
	}
	return nil
}
