package handlers_test

import (
	"errors"
	"testing"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/handlers"
)

func TestChangeDUStateDecodesEveryOperation(t *testing.T) {
	t.Parallel()
	var got handlers.DUStateRequest
	h := handlers.NewChangeDUState(func(r handlers.DUStateRequest) { got = r })
	req := `<ChangeDUState>
  <Operations soap-enc:arrayType="cwmp:OperationStruct[3]">
    <InstallOpStruct>
      <URL>http://apps.example/home-hub-1.yaml</URL>
      <UUID>11111111-2222-5333-8444-555555555555</UUID>
      <Username>u</Username>
      <Password>p</Password>
      <ExecutionEnvRef>Device.SoftwareModules.ExecEnv.1</ExecutionEnvRef>
    </InstallOpStruct>
    <UpdateOpStruct>
      <UUID>11111111-2222-5333-8444-555555555556</UUID>
      <Version>1.0.0</Version>
      <URL>http://apps.example/other-2.yaml</URL>
      <Username/>
      <Password/>
    </UpdateOpStruct>
    <UninstallOpStruct>
      <UUID>11111111-2222-5333-8444-555555555557</UUID>
      <Version/>
      <ExecutionEnvRef/>
    </UninstallOpStruct>
  </Operations>
  <CommandKey>batch-1</CommandKey>
</ChangeDUState>`
	out, err := invokeHandler(t, h, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("ChangeDUStateResponse carries no body, got %q", out)
	}
	if got.CommandKey != "batch-1" || len(got.Operations) != 3 {
		t.Fatalf("request = %+v", got)
	}
	in, up, un := got.Operations[0], got.Operations[1], got.Operations[2]
	if in.Kind != "install" || in.URL != "http://apps.example/home-hub-1.yaml" || in.UUID != "11111111-2222-5333-8444-555555555555" || in.ExecutionEnvRef != "Device.SoftwareModules.ExecEnv.1" {
		t.Errorf("install = %+v", in)
	}
	if up.Kind != "update" || up.UUID != "11111111-2222-5333-8444-555555555556" || up.Version != "1.0.0" || up.URL != "http://apps.example/other-2.yaml" {
		t.Errorf("update = %+v", up)
	}
	if un.Kind != "uninstall" || un.UUID != "11111111-2222-5333-8444-555555555557" || un.Version != "" {
		t.Errorf("uninstall = %+v", un)
	}
}

func TestChangeDUStateFaults(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"no operations":     `<ChangeDUState><Operations/><CommandKey>k</CommandKey></ChangeDUState>`,
		"install no url":    `<ChangeDUState><Operations><InstallOpStruct><UUID>x</UUID></InstallOpStruct></Operations></ChangeDUState>`,
		"uninstall no uuid": `<ChangeDUState><Operations><UninstallOpStruct><Version>1</Version></UninstallOpStruct></Operations></ChangeDUState>`,
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			called := false
			h := handlers.NewChangeDUState(func(handlers.DUStateRequest) { called = true })
			_, err := invokeHandler(t, h, req)
			var fe *cwmp.FaultError
			if !errors.As(err, &fe) || fe.Fault.FaultCode != 9003 {
				t.Fatalf("want fault 9003, got %v", err)
			}
			if called {
				t.Fatal("a faulted request must not be scheduled")
			}
		})
	}
}
