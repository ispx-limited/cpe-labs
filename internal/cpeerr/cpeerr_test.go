package cpeerr_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

var errBoom = errors.New("boom")

func TestWrap(t *testing.T) {
	t.Parallel()

	got := cpeerr.Wrap("tree.Get", cpeerr.KindNotFound, errBoom)
	if got == nil {
		t.Fatal("Wrap returned nil")
	}
	if got.Op != "tree.Get" {
		t.Errorf("Op = %q, want %q", got.Op, "tree.Get")
	}
	if got.Kind != cpeerr.KindNotFound {
		t.Errorf("Kind = %v, want %v", got.Kind, cpeerr.KindNotFound)
	}
	if got.Err != errBoom {
		t.Errorf("Err = %v, want %v", got.Err, errBoom)
	}
	if got.FaultCode != 0 {
		t.Errorf("FaultCode = %d, want 0 (reserved for protocol use)", got.FaultCode)
	}
}

func TestWrapNilCause(t *testing.T) {
	t.Parallel()

	got := cpeerr.Wrap("tree.Get", cpeerr.KindInvalidArgument, nil)
	if got.Err != nil {
		t.Errorf("Err = %v, want nil", got.Err)
	}
	if got.Error() == "" {
		t.Error("Error() returned empty string")
	}
}

func TestIs(t *testing.T) {
	t.Parallel()

	err := cpeerr.Wrap("tree.Get", cpeerr.KindNotFound, errBoom)

	if !cpeerr.Is(err, cpeerr.KindNotFound) {
		t.Error("cpeerr.Is(err, KindNotFound) = false, want true")
	}
	if cpeerr.Is(err, cpeerr.KindInternal) {
		t.Error("cpeerr.Is(err, KindInternal) = true, want false")
	}
	if cpeerr.Is(nil, cpeerr.KindNotFound) {
		t.Error("cpeerr.Is(nil, ...) = true, want false")
	}
	if cpeerr.Is(errBoom, cpeerr.KindNotFound) {
		t.Error("cpeerr.Is(plain error, ...) = true, want false")
	}
}

func TestStdlibErrorsIs(t *testing.T) {
	t.Parallel()

	err := cpeerr.Wrap("tree.Get", cpeerr.KindNotFound, errBoom)

	if !errors.Is(err, errBoom) {
		t.Error("errors.Is(wrapped, errBoom) = false, want true (Unwrap should reach the cause)")
	}
}

func TestStdlibErrorsAs(t *testing.T) {
	t.Parallel()

	err := cpeerr.Wrap("tree.Get", cpeerr.KindInvalidArgument, errBoom)

	var typed *cpeerr.Error
	if !errors.As(err, &typed) {
		t.Fatal("errors.As(err, *cpeerr.Error) = false, want true")
	}
	if typed.Kind != cpeerr.KindInvalidArgument {
		t.Errorf("typed.Kind = %v, want %v", typed.Kind, cpeerr.KindInvalidArgument)
	}
}

func TestWithFaultCode(t *testing.T) {
	t.Parallel()

	original := cpeerr.Wrap("rpc.Inform", cpeerr.KindInternal, errBoom)
	if original.FaultCode != 0 {
		t.Fatalf("baseline FaultCode = %d, want 0", original.FaultCode)
	}

	withCode := cpeerr.WithFaultCode(original, 9001)
	if withCode == nil {
		t.Fatal("WithFaultCode returned nil for non-nil input")
	}
	if withCode.FaultCode != 9001 {
		t.Errorf("FaultCode = %d, want 9001", withCode.FaultCode)
	}
	if original.FaultCode != 0 {
		t.Errorf("original mutated: FaultCode = %d, want 0", original.FaultCode)
	}
	if withCode == original {
		t.Error("WithFaultCode returned the same pointer; expected a copy")
	}
}

func TestWithFaultCodeNil(t *testing.T) {
	t.Parallel()

	if got := cpeerr.WithFaultCode(nil, 9001); got != nil {
		t.Errorf("WithFaultCode(nil, ...) = %v, want nil", got)
	}
}

func TestUnwrap(t *testing.T) {
	t.Parallel()

	err := cpeerr.Wrap("tree.Get", cpeerr.KindNotFound, errBoom)
	if got := errors.Unwrap(err); got != errBoom {
		t.Errorf("Unwrap = %v, want %v", got, errBoom)
	}
}

func TestKindString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind cpeerr.Kind
		want string
	}{
		{cpeerr.KindUnknown, "unknown"},
		{cpeerr.KindInvalidArgument, "invalid_argument"},
		{cpeerr.KindNotFound, "not_found"},
		{cpeerr.KindInternal, "internal"},
	}
	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}
