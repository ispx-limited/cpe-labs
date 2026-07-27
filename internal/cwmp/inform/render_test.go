package inform_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

func deviceID() inform.DeviceID {
	return inform.DeviceID{
		Manufacturer: "ACME",
		OUI:          "001122",
		ProductClass: "HomeGateway",
		SerialNumber: "ABC123",
	}
}

func TestRenderGoldens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		fixture string
		inf     inform.Inform
	}{
		{
			name:    "bootstrap",
			fixture: "inform_bootstrap.xml",
			inf: inform.Inform{
				DeviceID:     deviceID(),
				Events:       []inform.Event{{EventCode: inform.EventBootstrap}},
				MaxEnvelopes: 1,
				CurrentTime:  fixedTime,
				RetryCount:   0,
				Parameters: []inform.Parameter{
					{Name: "Device.DeviceInfo.SerialNumber", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "ABC123"}},
					{Name: "Device.ManagementServer.URL", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "http://acs.example/cwmp"}},
				},
			},
		},
		{
			name:    "periodic",
			fixture: "inform_periodic.xml",
			inf: inform.Inform{
				DeviceID:     deviceID(),
				Events:       []inform.Event{{EventCode: inform.EventPeriodic}},
				MaxEnvelopes: 1,
				CurrentTime:  fixedTime,
				RetryCount:   0,
				Parameters: []inform.Parameter{
					{Name: "Device.DeviceInfo.UpTime", Value: paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "3600"}},
					{Name: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "home"}},
				},
			},
		},
		{
			name:    "value_change",
			fixture: "inform_value_change.xml",
			inf: inform.Inform{
				DeviceID:     deviceID(),
				Events:       []inform.Event{{EventCode: inform.EventValueChange}},
				MaxEnvelopes: 1,
				CurrentTime:  fixedTime,
				RetryCount:   0,
				Parameters: []inform.Parameter{
					{Name: "Device.WiFi.SSID", Value: paramtree.Value{Type: paramtree.TypeString, Raw: "guest"}},
				},
			},
		},
		{
			name:    "connection_request",
			fixture: "inform_connection_request.xml",
			inf: inform.Inform{
				DeviceID: deviceID(),
				Events: []inform.Event{
					{EventCode: inform.EventConnectionRequest},
					{EventCode: inform.EventPeriodic},
				},
				MaxEnvelopes: 1,
				CurrentTime:  fixedTime,
				RetryCount:   0,
				Parameters: []inform.Parameter{
					{Name: "Device.DeviceInfo.UpTime", Value: paramtree.Value{Type: paramtree.TypeUnsignedInt, Raw: "3600"}},
				},
			},
		},
		{
			name:    "method_reboot",
			fixture: "inform_method_reboot.xml",
			inf: inform.Inform{
				DeviceID: deviceID(),
				Events: []inform.Event{
					{EventCode: inform.EventMethodReboot, CommandKey: "ops-2026-04-28"},
					{EventCode: inform.EventBoot},
				},
				MaxEnvelopes: 1,
				CurrentTime:  fixedTime,
				RetryCount:   0,
			},
		},
		{
			name:    "no_event_match",
			fixture: "inform_no_event_match.xml",
			inf: inform.Inform{
				DeviceID:     deviceID(),
				Events:       []inform.Event{{EventCode: inform.EventDiagnostics}},
				MaxEnvelopes: 1,
				CurrentTime:  fixedTime,
				RetryCount:   3,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := inform.Render(&buf, &tc.inf); err != nil {
				t.Fatalf("Render: %v", err)
			}
			testgolden.Compare(t, tc.fixture, buf.Bytes())
		})
	}
}

func TestRenderXMLEscapesText(t *testing.T) {
	t.Parallel()

	inf := inform.Inform{
		DeviceID:     inform.DeviceID{Manufacturer: `<bad> & "stuff"`, SerialNumber: "x'y"},
		Events:       []inform.Event{{EventCode: inform.EventPeriodic}},
		MaxEnvelopes: 1,
		CurrentTime:  fixedTime,
	}
	var buf bytes.Buffer
	if err := inform.Render(&buf, &inf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, `<bad>`) {
		t.Errorf("Manufacturer not escaped: %s", out)
	}
	if !strings.Contains(out, `&lt;bad&gt;`) || !strings.Contains(out, `&amp;`) || !strings.Contains(out, `&apos;`) {
		t.Errorf("expected XML-escaped output, got:\n%s", out)
	}
}

func TestRenderEmptyParameterList(t *testing.T) {
	t.Parallel()

	inf := inform.Inform{
		DeviceID:     deviceID(),
		Events:       []inform.Event{{EventCode: inform.EventPeriodic}},
		MaxEnvelopes: 1,
		CurrentTime:  fixedTime,
	}
	var buf bytes.Buffer
	if err := inform.Render(&buf, &inf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `<ParameterList></ParameterList>`) {
		t.Errorf("expected empty <ParameterList></ParameterList>; got:\n%s", buf.String())
	}
}

func TestRenderMethodEventCommandKey(t *testing.T) {
	t.Parallel()

	inf := inform.Inform{
		DeviceID:     deviceID(),
		Events:       []inform.Event{{EventCode: inform.EventMethodReboot, CommandKey: "ops-42"}},
		MaxEnvelopes: 1,
		CurrentTime:  fixedTime,
	}
	var buf bytes.Buffer
	if err := inform.Render(&buf, &inf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `<CommandKey>ops-42</CommandKey>`) {
		t.Errorf("CommandKey missing from output:\n%s", buf.String())
	}
}

func TestRenderNilRejected(t *testing.T) {
	t.Parallel()

	if err := inform.Render(&bytes.Buffer{}, nil); err == nil {
		t.Fatal("expected error for nil Inform")
	}
}
