package paramtree_test

import (
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestValidateString(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "hello", "héllo世界", "\x00\x01\x02"} {
		if err := paramtree.Validate(paramtree.TypeString, raw); err != nil {
			t.Errorf("Validate(string, %q) = %v, want nil", raw, err)
		}
	}
	// invalid UTF-8 byte sequence
	if err := paramtree.Validate(paramtree.TypeString, "\xff\xfe"); err == nil {
		t.Error("Validate(string, invalid UTF-8) returned nil, want error")
	}
}

func TestValidateInt32(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"-2147483648", "0", "2147483647"} {
		if err := paramtree.Validate(paramtree.TypeInt, raw); err != nil {
			t.Errorf("Validate(int, %q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"-2147483649", "2147483648", "abc", "", "1.5"} {
		if err := paramtree.Validate(paramtree.TypeInt, raw); err == nil {
			t.Errorf("Validate(int, %q) returned nil, want error", raw)
		}
	}
}

func TestValidateUnsignedInt32(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"0", "1", "4294967295"} {
		if err := paramtree.Validate(paramtree.TypeUnsignedInt, raw); err != nil {
			t.Errorf("Validate(unsignedInt, %q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"-1", "4294967296", "abc", ""} {
		if err := paramtree.Validate(paramtree.TypeUnsignedInt, raw); err == nil {
			t.Errorf("Validate(unsignedInt, %q) returned nil, want error", raw)
		}
	}
}

func TestValidateBoolean(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"0", "1", "true", "false"} {
		if err := paramtree.Validate(paramtree.TypeBoolean, raw); err != nil {
			t.Errorf("Validate(boolean, %q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"True", "FALSE", "yes", "no", "", "2"} {
		if err := paramtree.Validate(paramtree.TypeBoolean, raw); err == nil {
			t.Errorf("Validate(boolean, %q) returned nil, want error", raw)
		}
	}
}

func TestValidateDateTime(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"2026-04-28T12:00:00Z",
		"2026-04-28T12:00:00+00:00",
		"2026-04-28T12:00:00-08:00",
		"2026-04-28T12:00:00.123456789Z",
	} {
		if err := paramtree.Validate(paramtree.TypeDateTime, raw); err != nil {
			t.Errorf("Validate(dateTime, %q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{
		"2026-04-28T12:00:00", // no TZ
		"2026-04-28",          // date only
		"yesterday",           // gibberish
		"",                    // empty
	} {
		if err := paramtree.Validate(paramtree.TypeDateTime, raw); err == nil {
			t.Errorf("Validate(dateTime, %q) returned nil, want error", raw)
		}
	}
}

func TestValidateBase64(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "SGVsbG8=", "SGVsbG8gV29ybGQ=", "AAECAwQF"} {
		if err := paramtree.Validate(paramtree.TypeBase64, raw); err != nil {
			t.Errorf("Validate(base64, %q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"SGVsbG8", "not base64!", "===="} {
		if err := paramtree.Validate(paramtree.TypeBase64, raw); err == nil {
			t.Errorf("Validate(base64, %q) returned nil, want error", raw)
		}
	}
}

func TestValidateUnknownType(t *testing.T) {
	t.Parallel()

	err := paramtree.Validate(paramtree.Type("xsd:integer"), "42")
	if err == nil {
		t.Fatal("Validate(unknown type) returned nil, want error")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}

func TestMarshalIdentity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		typ paramtree.Type
		raw string
	}{
		{paramtree.TypeString, "hello"},
		{paramtree.TypeInt, "-42"},
		{paramtree.TypeUnsignedInt, "65535"},
		{paramtree.TypeBase64, "SGVsbG8="},
	}
	for _, tc := range cases {
		got, err := paramtree.Marshal(tc.typ, tc.raw)
		if err != nil {
			t.Errorf("Marshal(%s, %q) = %v", tc.typ, tc.raw, err)
			continue
		}
		if got != tc.raw {
			t.Errorf("Marshal(%s, %q) = %q, want unchanged", tc.typ, tc.raw, got)
		}
	}
}

func TestMarshalBooleanCanonicalizes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"0":     "false",
		"1":     "true",
		"true":  "true",
		"false": "false",
	}
	for in, want := range cases {
		got, err := paramtree.Marshal(paramtree.TypeBoolean, in)
		if err != nil {
			t.Errorf("Marshal(boolean, %q) = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Marshal(boolean, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarshalDateTimeNormalizesToUTC(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"2026-04-28T12:00:00+00:00": "2026-04-28T12:00:00Z",
		"2026-04-28T12:00:00Z":      "2026-04-28T12:00:00Z",
		"2026-04-28T12:00:00-08:00": "2026-04-28T20:00:00Z",
	}
	for in, want := range cases {
		got, err := paramtree.Marshal(paramtree.TypeDateTime, in)
		if err != nil {
			t.Errorf("Marshal(dateTime, %q) = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Marshal(dateTime, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarshalGoldens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		typ     paramtree.Type
		raw     string
		fixture string
	}{
		{"string", paramtree.TypeString, "hello world", "marshal_string.golden"},
		{"int", paramtree.TypeInt, "-2147483648", "marshal_int.golden"},
		{"unsigned_int", paramtree.TypeUnsignedInt, "4294967295", "marshal_unsigned_int.golden"},
		{"boolean", paramtree.TypeBoolean, "1", "marshal_boolean.golden"},
		{"datetime", paramtree.TypeDateTime, "2026-04-28T12:00:00+00:00", "marshal_datetime.golden"},
		{"base64", paramtree.TypeBase64, "SGVsbG8gV29ybGQ=", "marshal_base64.golden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := paramtree.Marshal(tc.typ, tc.raw)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			testgolden.Compare(t, tc.fixture, []byte(got))
		})
	}
}

func TestSetRejectsBadRaw(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	must(t, tree.Mount("Device.Counter", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeInt, Raw: "0", Writable: true,
	})))

	err := tree.Set("Device.Counter", paramtree.Value{
		Type: paramtree.TypeInt, Raw: "abc", Writable: true,
	})
	if err == nil {
		t.Fatal("Set with bad Raw returned nil, want error")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v, want KindInvalidArgument", err)
	}

	// Tree should still hold the original valid value.
	v, _ := tree.Get("Device.Counter")
	if v.Raw != "0" {
		t.Errorf("Get after rejected Set = %q, want %q (no partial mutation)", v.Raw, "0")
	}
}

func TestSetStoresRawAsPassedNotCanonical(t *testing.T) {
	t.Parallel()

	tree := paramtree.New()
	must(t, tree.Mount("Device.WiFi.Enable", paramtree.NewLeaf(paramtree.Value{
		Type: paramtree.TypeBoolean, Raw: "true", Writable: true,
	})))

	// Set with the lexical form "0", Validate accepts it; the tree
	// stores "0" (not canonicalized to "false"). Marshal at render time
	// would produce "false".
	if err := tree.Set("Device.WiFi.Enable", paramtree.Value{
		Type: paramtree.TypeBoolean, Raw: "0", Writable: true,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, _ := tree.Get("Device.WiFi.Enable")
	if v.Raw != "0" {
		t.Errorf("Get = %q, want %q (Set must not canonicalize)", v.Raw, "0")
	}
}
