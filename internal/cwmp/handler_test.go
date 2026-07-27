package cwmp_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
)

func TestFaultErrorIsError(t *testing.T) {
	t.Parallel()

	var err error = &cwmp.FaultError{Fault: soap.Fault{FaultCode: 9005, FaultString: "bad path"}}
	if !strings.Contains(err.Error(), "9005") {
		t.Errorf("Error() = %q, want it to mention 9005", err.Error())
	}
	if !strings.Contains(err.Error(), "bad path") {
		t.Errorf("Error() = %q, want it to mention the FaultString", err.Error())
	}
}

func TestFaultErrorNilSafeError(t *testing.T) {
	t.Parallel()

	var fe *cwmp.FaultError
	got := fe.Error()
	if got == "" || !strings.Contains(got, "nil") {
		t.Errorf("nil FaultError Error() = %q, want non-empty mentioning nil", got)
	}
}

func TestFaultErrorErrorsAs(t *testing.T) {
	t.Parallel()

	original := &cwmp.FaultError{Fault: soap.Fault{FaultCode: 9007, FaultString: "type mismatch"}}
	// Wrap through fmt.Errorf to confirm errors.As walks chains.
	wrapped := fmt.Errorf("dispatch failed: %w", original)

	var got *cwmp.FaultError
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As did not extract FaultError from wrapped error")
	}
	if got.Fault.FaultCode != 9007 {
		t.Errorf("Fault.FaultCode = %d, want 9007", got.Fault.FaultCode)
	}
}

func TestNewSessionRequiresTransport(t *testing.T) {
	t.Parallel()

	_, err := cwmp.NewSession(cwmp.SessionOptions{})
	if err == nil {
		t.Fatal("expected error for nil Transport")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("kind = %v", err)
	}
}
