package paramtree

import "testing"

func TestLoadExampleArrisWithNetDiagnostics(t *testing.T) {
	prof, err := loadProfileDir("../../profiles/example-arris")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(prof.Diagnostics) != 6 {
		t.Fatalf("diagnostics = %d, want 6 (scan, net trio, self test, channel scan)", len(prof.Diagnostics))
	}
	for _, p := range []string{
		"InternetGatewayDevice.IPPingDiagnostics.Host",
		"InternetGatewayDevice.IPPingDiagnostics.X_0000C5_IPv6Preferred",
		"InternetGatewayDevice.TraceRouteDiagnostics.RouteHops.6.HopRTTimes",
		"InternetGatewayDevice.NSLookupDiagnostics.Result.1.IPAddresses",
		"InternetGatewayDevice.LANDevice.1.X_0000C5_Wireless.ChannelDiagnostics.Result.13.ChannelScore",
		"InternetGatewayDevice.SelfTestDiagnostics.Results",
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
	if len(prof.Diagnostics) != 8 {
		t.Fatalf("diagnostics = %d, want 8 (net trio plus the second wave's five)", len(prof.Diagnostics))
	}
	for _, p := range []string{
		"Device.IP.Diagnostics.IPPing.ProtocolVersion",
		"Device.IP.Diagnostics.TraceRoute.RouteHops.6.RTTimes",
		"Device.DNS.Diagnostics.NSLookupDiagnostics.Result.1.IPAddresses",
		"Device.SelfTestDiagnostics.Results",
		"Device.IP.Diagnostics.ServerSelectionDiagnostics.FastestHost",
		"Device.IP.Diagnostics.UDPEchoDiagnostics.IndividualPacketResult.20.PacketReceiveTime",
		"Device.Users.CheckCredentialsDiagnostics.Password",
		"Device.IP.Diagnostics.IPLayerCapacityMetrics.MaxIPLayerCapacity",
	} {
		if _, err := prof.Tree.Get(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestLoadAvmWithDslLineTest(t *testing.T) {
	prof, err := loadProfileDir("../../profiles/example-avm-fritzbox-7690")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := prof.Tree.Get("Device.DSL.Diagnostics.ADSLLineTest.HLOGpsds"); err != nil {
		t.Errorf("missing HLOGpsds: %v", err)
	}
}
