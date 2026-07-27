package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func TestGPNNextLevelTrue(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterNames(buildHandlerTree(t))
	req := `<GetParameterNames>
  <ParameterPath>Device.WiFi.AccessPoint.</ParameterPath>
  <NextLevel>true</NextLevel>
</GetParameterNames>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatal(err)
	}
	testgolden.Compare(t, "gpn_next_level_true.xml", out)
}

func TestGPNNextLevelFalse(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterNames(buildHandlerTree(t))
	req := `<GetParameterNames>
  <ParameterPath>Device.WiFi.AccessPoint.</ParameterPath>
  <NextLevel>false</NextLevel>
</GetParameterNames>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatal(err)
	}
	testgolden.Compare(t, "gpn_next_level_false.xml", out)
}

func TestGPNUnknownPath(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterNames(buildHandlerTree(t))
	req := `<GetParameterNames>
  <ParameterPath>Device.DoesNotExist.</ParameterPath>
  <NextLevel>true</NextLevel>
</GetParameterNames>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault for unknown path")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9005 {
		t.Errorf("expected fault 9005; got: %v", err)
	}
}
