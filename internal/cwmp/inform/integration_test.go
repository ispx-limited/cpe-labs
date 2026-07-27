package inform_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/testgolden"
)

// TestEnvelopeRoundTripPeriodic wires soap.Encoder + inform.Render
// together to produce a full envelope, decodes it back, and verifies
// the framing round-trips. The full-envelope bytes are also locked
// down as a golden so future changes to either soap or inform are
// caught at integration boundaries.
func TestEnvelopeRoundTripPeriodic(t *testing.T) {
	t.Parallel()

	tree := buildTree(t)
	b, err := inform.NewBuilder(tree, inform.BuilderOptions{
		Clock:         fixedClock,
		DeviceIDPaths: testDeviceIDPaths,
		ParameterLists: map[string][]string{
			inform.EventPeriodic: {"Device.DeviceInfo.UpTime", "Device.WiFi.SSID"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inf, err := b.Build([]inform.Event{{EventCode: inform.EventPeriodic}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, soap.EncoderOptions{})
	mw, err := enc.WriteRequest(soap.Header{ID: "42"}, "Inform")
	if err != nil {
		t.Fatal(err)
	}
	bodyBuf := &bytes.Buffer{}
	if rerr := inform.Render(bodyBuf, inf); rerr != nil {
		t.Fatalf("Render: %v", rerr)
	}
	if rawErr := mw.Raw(bodyBuf.Bytes()); rawErr != nil {
		t.Fatalf("Raw: %v", rawErr)
	}
	if closeErr := mw.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	testgolden.Compare(t, "full_envelope_periodic.xml", buf.Bytes())

	// Round-trip: decode the freshly-built envelope and verify framing.
	d := soap.NewDecoder(&buf, soap.DecoderOptions{})
	env, err := d.ReadEnvelope()
	if err != nil {
		t.Fatalf("ReadEnvelope: %v", err)
	}
	if env.Method != "Inform" {
		t.Errorf("Method = %q, want Inform", env.Method)
	}
	if env.Header.ID != "42" {
		t.Errorf("Header.ID = %q, want %q", env.Header.ID, "42")
	}
	if env.IsFault {
		t.Error("IsFault should be false")
	}
	tr, err := d.MethodTokens()
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := tr.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
