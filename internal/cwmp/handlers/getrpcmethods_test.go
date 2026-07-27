package handlers_test

import (
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
)

func TestGetRPCMethods(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetRPCMethods([]string{"GetRPCMethods", "GetParameterValues", "Reboot"})
	if h.Method() != "GetRPCMethods" {
		t.Fatalf("Method() = %q", h.Method())
	}
	out, err := invokeHandler(t, h, "<GetRPCMethods></GetRPCMethods>")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `soap-enc:arrayType="xsd:string[3]"`) {
		t.Errorf("missing arrayType: %s", s)
	}
	for _, m := range []string{"GetRPCMethods", "GetParameterValues", "Reboot"} {
		if !strings.Contains(s, "<string>"+m+"</string>") {
			t.Errorf("MethodList missing %s", m)
		}
	}
}
