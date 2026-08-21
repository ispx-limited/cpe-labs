package dustate_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/dustate"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestRender(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c := &dustate.Complete{
		CommandKey: "install-home-hub",
		Results: []dustate.OpResult{
			{
				UUID:                 "11111111-2222-5333-8444-555555555555",
				DeploymentUnitRef:    "Device.SoftwareModules.DeploymentUnit.1",
				Version:              "1.0.0",
				CurrentState:         "Installed",
				Resolved:             true,
				ExecutionUnitRefList: "Device.SoftwareModules.ExecutionUnit.1",
				StartTime:            start,
				CompleteTime:         start.Add(5 * time.Second),
			},
			{
				UUID:         "11111111-2222-5333-8444-555555555556",
				CurrentState: "Failed",
				StartTime:    start,
				CompleteTime: start.Add(time.Second),
				FaultCode:    9018,
				FaultString:  "File corrupted <signature>",
			},
		},
	}
	var buf bytes.Buffer
	if err := dustate.Render(&buf, c); err != nil {
		t.Fatalf("Render: %v", err)
	}
	testgolden.Compare(t, "dustatechangecomplete.xml", buf.Bytes())
}

func TestRenderNil(t *testing.T) {
	t.Parallel()
	if err := dustate.Render(&bytes.Buffer{}, nil); err == nil {
		t.Fatal("expected an error")
	}
}
