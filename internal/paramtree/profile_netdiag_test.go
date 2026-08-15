package paramtree

import "testing"

func TestLoadExampleArrisWithNetDiagnostics(t *testing.T) {
	prof, err := loadProfileDir("../../profiles/example-arris")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(prof.Diagnostics) != 4 {
		t.Fatalf("diagnostics = %d, want 4 (scan + ping + traceroute + nslookup)", len(prof.Diagnostics))
	}
	for _, p := range []string{
		"InternetGatewayDevice.IPPingDiagnostics.Host",
		"InternetGatewayDevice.IPPingDiagnostics.X_0000C5_IPv6Preferred",
		"InternetGatewayDevice.TraceRouteDiagnostics.RouteHops.6.HopRTTimes",
		"InternetGatewayDevice.NSLookupDiagnostics.Result.1.IPAddresses",
	} {
		if _, err := prof.Tree.Get(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestLoadSagemcomWithNetDiagnostics(t *testing.T) {
	prof, err := loadProfileDir("../../profiles/example-sagemcom-fast5598")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(prof.Diagnostics) != 3 {
		t.Fatalf("diagnostics = %d, want 3", len(prof.Diagnostics))
	}
	for _, p := range []string{
		"Device.IP.Diagnostics.IPPing.ProtocolVersion",
		"Device.IP.Diagnostics.TraceRoute.RouteHops.6.RTTimes",
		"Device.DNS.Diagnostics.NSLookupDiagnostics.Result.1.IPAddresses",
	} {
		if _, err := prof.Tree.Get(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}
