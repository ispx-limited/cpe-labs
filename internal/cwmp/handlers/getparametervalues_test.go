package handlers_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

// buildHandlerTree returns a small TR-181-shaped tree used across
// handler tests. Pinning the shape keeps goldens stable.
func buildHandlerTree(t *testing.T) *paramtree.Tree {
	t.Helper()
	tree := paramtree.New()
	for _, leaf := range []struct {
		path string
		v    paramtree.Value
	}{
		{"Device.DeviceInfo.Manufacturer", paramtree.Value{Type: paramtree.TypeString, Raw: "ACME"}},
		{"Device.DeviceInfo.SerialNumber", paramtree.Value{Type: paramtree.TypeString, Raw: "ABC123"}},
		{"Device.DeviceInfo.UpTime", paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "3600"}},
		{"Device.WiFi.AccessPoint.1.SSID", paramtree.Value{Type: paramtree.TypeString, Raw: "home", Writable: true}},
		{"Device.WiFi.AccessPoint.1.Enable", paramtree.Value{Type: paramtree.TypeBoolean, Raw: "true", Writable: true}},
		{"Device.WiFi.AccessPoint.2.SSID", paramtree.Value{Type: paramtree.TypeString, Raw: "guest", Writable: true}},
		{"Device.WiFi.AccessPoint.2.Enable", paramtree.Value{Type: paramtree.TypeBoolean, Raw: "false", Writable: true}},
	} {
		if err := tree.Mount(leaf.path, paramtree.NewLeaf(leaf.v)); err != nil {
			t.Fatalf("mount %s: %v", leaf.path, err)
		}
	}
	return tree
}

// invokeHandler builds an xml.TokenReader from a request body string,
// runs the handler, and returns the response bytes (or any error).
func invokeHandler(t *testing.T, h cwmp.Handler, requestBody string) ([]byte, error) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(requestBody))
	// Position the decoder just past the wrapping <method> element so
	// the handler sees only the body content.
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("invokeHandler: %v", err)
		}
		if _, ok := tok.(xml.StartElement); ok {
			break
		}
	}
	var out bytes.Buffer
	err := h.Handle(context.Background(), dec, &out)
	return out.Bytes(), err
}

func TestGPVSinglePath(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterValues(buildHandlerTree(t))
	req := `<GetParameterValues>
  <ParameterNames>
    <string>Device.DeviceInfo.SerialNumber</string>
  </ParameterNames>
</GetParameterValues>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "gpv_single_path.xml", out)
}

func TestGPVPartialPath(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterValues(buildHandlerTree(t))
	req := `<GetParameterValues>
  <ParameterNames>
    <string>Device.WiFi.AccessPoint.</string>
  </ParameterNames>
</GetParameterValues>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	testgolden.Compare(t, "gpv_partial_path.xml", out)
}

func TestGPVUnknownPath(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterValues(buildHandlerTree(t))
	req := `<GetParameterValues>
  <ParameterNames>
    <string>Device.DoesNotExist</string>
  </ParameterNames>
</GetParameterValues>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault for unknown path")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FaultError, got %T", err)
	}
	if fe.Fault.FaultCode != 9005 {
		t.Errorf("FaultCode = %d, want 9005", fe.Fault.FaultCode)
	}
}

func TestGPVEmptyListRejected(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterValues(buildHandlerTree(t))
	req := `<GetParameterValues>
  <ParameterNames>
  </ParameterNames>
</GetParameterValues>`
	_, err := invokeHandler(t, h, req)
	if err == nil {
		t.Fatal("expected fault for empty list")
	}
	var fe *cwmp.FaultError
	if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
		t.Errorf("expected fault 9003, got: %v", err)
	}
}

func TestGPVMixedTypesMarshal(t *testing.T) {
	t.Parallel()

	h := handlers.NewGetParameterValues(buildHandlerTree(t))
	req := `<GetParameterValues>
  <ParameterNames>
    <string>Device.DeviceInfo.UpTime</string>
    <string>Device.WiFi.AccessPoint.1.Enable</string>
  </ParameterNames>
</GetParameterValues>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, `xsi:type="xsd:unsignedInt"`) {
		t.Errorf("UpTime should render xsi:type=xsd:unsignedInt; got:\n%s", body)
	}
	if !strings.Contains(body, `xsi:type="xsd:boolean"`) {
		t.Errorf("Enable should render xsi:type=xsd:boolean; got:\n%s", body)
	}
}
