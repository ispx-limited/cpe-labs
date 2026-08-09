package cpeconfig_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeconfig"
	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load(nil, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "text")
	}
	if cfg.Concurrency != 1 {
		t.Errorf("Concurrency = %d, want 1", cfg.Concurrency)
	}
	if cfg.Seed != 0 {
		t.Errorf("Seed = %d, want 0", cfg.Seed)
	}
	if cfg.ACSURL != "" || cfg.ProfilePath != "" || cfg.ConfigPath != "" {
		t.Errorf("non-empty default for an unset string field: %+v", cfg)
	}
}

func TestLoadFlagsOverrideDefaults(t *testing.T) {
	t.Parallel()

	args := []string{
		"--acs-url=http://acs/cwmp",
		"--log-level=debug",
		"--log-format=json",
		"--concurrency=8",
		"--seed=42",
		"--profile=profiles/x.yaml",
	}
	cfg, err := cpeconfig.Load(args, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSURL != "http://acs/cwmp" {
		t.Errorf("ACSURL = %q", cfg.ACSURL)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q", cfg.LogFormat)
	}
	if cfg.Concurrency != 8 {
		t.Errorf("Concurrency = %d", cfg.Concurrency)
	}
	if cfg.Seed != 42 {
		t.Errorf("Seed = %d", cfg.Seed)
	}
	if cfg.ProfilePath != "profiles/x.yaml" {
		t.Errorf("ProfilePath = %q", cfg.ProfilePath)
	}
}

func TestLoadEnvOverridesDefaults(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"CPE_SIM_ACS_URL":     "http://env/cwmp",
		"CPE_SIM_LOG_LEVEL":   "warn",
		"CPE_SIM_LOG_FORMAT":  "json",
		"CPE_SIM_CONCURRENCY": "16",
		"CPE_SIM_SEED":        "7",
		"CPE_SIM_PROFILE":     "profiles/env.yaml",
	}
	cfg, err := cpeconfig.Load(nil, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSURL != "http://env/cwmp" {
		t.Errorf("ACSURL = %q", cfg.ACSURL)
	}
	if cfg.LogLevel != "warn" || cfg.LogFormat != "json" {
		t.Errorf("level/format = %q/%q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.Concurrency != 16 {
		t.Errorf("Concurrency = %d", cfg.Concurrency)
	}
	if cfg.Seed != 7 {
		t.Errorf("Seed = %d", cfg.Seed)
	}
	if cfg.ProfilePath != "profiles/env.yaml" {
		t.Errorf("ProfilePath = %q", cfg.ProfilePath)
	}
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, `
acsURL: "http://file/cwmp"
logLevel: "error"
logFormat: "json"
concurrency: 32
seed: 13
profile: "profiles/file.yaml"
`)
	cfg, err := cpeconfig.Load([]string{"--config", path}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSURL != "http://file/cwmp" {
		t.Errorf("ACSURL = %q", cfg.ACSURL)
	}
	if cfg.LogLevel != "error" || cfg.LogFormat != "json" {
		t.Errorf("level/format = %q/%q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.Concurrency != 32 {
		t.Errorf("Concurrency = %d", cfg.Concurrency)
	}
	if cfg.Seed != 13 {
		t.Errorf("Seed = %d", cfg.Seed)
	}
	if cfg.ProfilePath != "profiles/file.yaml" {
		t.Errorf("ProfilePath = %q", cfg.ProfilePath)
	}
	if cfg.ConfigPath != path {
		t.Errorf("ConfigPath = %q, want %q", cfg.ConfigPath, path)
	}
}

func TestLoadFlagsOverrideEnv(t *testing.T) {
	t.Parallel()

	env := map[string]string{"CPE_SIM_ACS_URL": "http://env"}
	args := []string{"--acs-url=http://flag"}
	cfg, err := cpeconfig.Load(args, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSURL != "http://flag" {
		t.Errorf("flag should win: ACSURL = %q", cfg.ACSURL)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, `acsURL: "http://file"`)
	env := map[string]string{"CPE_SIM_ACS_URL": "http://env"}
	cfg, err := cpeconfig.Load([]string{"--config", path}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSURL != "http://env" {
		t.Errorf("env should win over file: ACSURL = %q", cfg.ACSURL)
	}
}

func TestLoadFileOverridesDefaults_NoEnvNoFlag(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, `concurrency: 4`)
	cfg, err := cpeconfig.Load([]string{"--config", path}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want 4", cfg.Concurrency)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	t.Parallel()

	const want = "http://round/cwmp"

	cfgFlag, err := cpeconfig.Load([]string{"--acs-url=" + want}, nil)
	if err != nil {
		t.Fatalf("flag load: %v", err)
	}
	cfgEnv, err := cpeconfig.Load(nil, map[string]string{"CPE_SIM_ACS_URL": want})
	if err != nil {
		t.Fatalf("env load: %v", err)
	}
	path := writeYAML(t, `acsURL: "`+want+`"`)
	cfgFile, err := cpeconfig.Load([]string{"--config", path}, nil)
	if err != nil {
		t.Fatalf("file load: %v", err)
	}

	if cfgFlag.ACSURL != want || cfgEnv.ACSURL != want || cfgFile.ACSURL != want {
		t.Errorf("round trip mismatch: flag=%q env=%q file=%q", cfgFlag.ACSURL, cfgEnv.ACSURL, cfgFile.ACSURL)
	}
}

func TestLoadUnknownFlagErrors(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{"--bogus"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestLoadUnknownEnvKeyErrors(t *testing.T) {
	t.Parallel()

	env := map[string]string{"CPE_SIM_FOOBAR": "1"}
	_, err := cpeconfig.Load(nil, env)
	if err == nil {
		t.Fatal("expected error for unknown env key")
	}
	if !strings.Contains(err.Error(), "CPE_SIM_FOOBAR") {
		t.Errorf("error should name the offending key: %v", err)
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("error kind = %v, want KindInvalidArgument", err)
	}
}

func TestLoadUnknownYAMLKeyErrors(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load(
		[]string{"--config", "testdata/unknown.yaml"},
		nil,
	)
	if err == nil {
		t.Fatal("expected error for unknown YAML key")
	}
	if !strings.Contains(err.Error(), "unsupportedKey") {
		t.Errorf("error should name the offending key: %v", err)
	}
}

func TestLoadInvalidLogLevel(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{"--log-level=bogus"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid log-level")
	}
	if !cpeerr.Is(err, cpeerr.KindInvalidArgument) {
		t.Errorf("error kind mismatch: %v", err)
	}
}

func TestLoadInvalidLogFormat(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{"--log-format=yaml"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid log-format")
	}
}

func TestLoadConcurrencyMustBePositive(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{"--concurrency=0"}, nil)
	if err == nil {
		t.Fatal("expected error for concurrency=0")
	}
}

func TestLoadMissingFileErrors(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{"--config", "/nonexistent/path/cpe-sim.yaml"}, nil)
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadHelpReturnsErrHelp(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{"--help"}, nil)
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("Load(--help) returned %v, want flag.ErrHelp", err)
	}
}

func TestLoadValidFixture(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load([]string{"--config", "testdata/valid.yaml"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSURL != "http://acs.example/cwmp" {
		t.Errorf("ACSURL = %q", cfg.ACSURL)
	}
	if cfg.ACSUsername != "cpe" || cfg.ACSPassword != "secret" {
		t.Errorf("ACS creds = %q/%q", cfg.ACSUsername, cfg.ACSPassword)
	}
	if cfg.ACSTimeout != 45*time.Second {
		t.Errorf("ACSTimeout = %v, want 45s", cfg.ACSTimeout)
	}
	if !cfg.TLSSkipVerify {
		t.Errorf("TLSSkipVerify = %v, want true", cfg.TLSSkipVerify)
	}
	if cfg.CACertFile != "/etc/ssl/ca.pem" {
		t.Errorf("CACertFile = %q", cfg.CACertFile)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("level/format = %q/%q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.Concurrency != 4 || cfg.Seed != 42 {
		t.Errorf("concurrency/seed = %d/%d", cfg.Concurrency, cfg.Seed)
	}
	if cfg.ProfilePath != "profiles/example.yaml" {
		t.Errorf("ProfilePath = %q", cfg.ProfilePath)
	}
}

func TestLoadACSCredentialsFromFlags(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load([]string{
		"--acs-url=http://acs.example/cwmp",
		"--acs-username=cpe",
		"--acs-password=secret",
		"--acs-timeout=15s",
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSUsername != "cpe" || cfg.ACSPassword != "secret" {
		t.Errorf("creds = %q/%q", cfg.ACSUsername, cfg.ACSPassword)
	}
	if cfg.ACSTimeout != 15*time.Second {
		t.Errorf("ACSTimeout = %v", cfg.ACSTimeout)
	}
}

func TestLoadACSCredentialsFromEnv(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"CPE_SIM_ACS_USERNAME": "cpe",
		"CPE_SIM_ACS_PASSWORD": "secret",
		"CPE_SIM_ACS_TIMEOUT":  "20s",
	}
	cfg, err := cpeconfig.Load(nil, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACSUsername != "cpe" || cfg.ACSPassword != "secret" {
		t.Errorf("creds = %q/%q", cfg.ACSUsername, cfg.ACSPassword)
	}
	if cfg.ACSTimeout != 20*time.Second {
		t.Errorf("ACSTimeout = %v", cfg.ACSTimeout)
	}
}

func TestLoadACSTimeoutDefault(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACSTimeout != 30*time.Second {
		t.Errorf("ACSTimeout default = %v, want 30s", cfg.ACSTimeout)
	}
}

func TestLoadACSTimeoutInvalid(t *testing.T) {
	t.Parallel()

	if _, err := cpeconfig.Load([]string{"--acs-timeout=bogus"}, nil); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestLoadACSTimeoutZeroRejected(t *testing.T) {
	t.Parallel()

	if _, err := cpeconfig.Load([]string{"--acs-timeout=0s"}, nil); err == nil {
		t.Fatal("expected error for zero timeout")
	}
}

func TestLoadTLSSkipVerify(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load([]string{"--tls-skip-verify=true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TLSSkipVerify {
		t.Errorf("TLSSkipVerify = %v, want true", cfg.TLSSkipVerify)
	}
}

func TestLoadCACertFileFromYAML(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load([]string{"--config", "testdata/valid.yaml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CACertFile != "/etc/ssl/ca.pem" {
		t.Errorf("CACertFile = %q", cfg.CACertFile)
	}
}

func TestEnvMap(t *testing.T) {
	t.Parallel()

	got := cpeconfig.EnvMap([]string{
		"A=1",
		"B=2=3",
		"NO_EQUALS",
		"=leading-empty-key",
		"C=",
	})
	want := map[string]string{
		"A": "1",
		"B": "2=3",
		"C": "",
	}
	if len(got) != len(want) {
		t.Fatalf("EnvMap len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("EnvMap[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestLoadCRDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CRBindAddr != "" {
		t.Errorf("default CRBindAddr = %q, want empty", cfg.CRBindAddr)
	}
	if cfg.CRPath != "/cr" {
		t.Errorf("default CRPath = %q, want /cr", cfg.CRPath)
	}
	if cfg.CRPublishPath != "" {
		t.Errorf("default CRPublishPath = %q, want empty (no TR-181 default, operator supplies)", cfg.CRPublishPath)
	}
}

func TestLoadCRPublishPathRequiredWhenBindAddrSet(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{
		"--cr-bind-addr", "127.0.0.1:7547",
		// no --cr-publish-path
	}, nil)
	if err == nil {
		t.Fatal("expected error when CRBindAddr is set without CRPublishPath")
	}
	if !strings.Contains(err.Error(), "cr-publish-path") {
		t.Errorf("error should mention cr-publish-path: %v", err)
	}
}

func TestLoadCRPublishPathEmptyOKWhenBindAddrEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CRBindAddr != "" || cfg.CRPublishPath != "" {
		t.Errorf("zero-value Config = %+v", cfg)
	}
}

func TestLoadCRFlagOverride(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load([]string{
		"--cr-bind-addr", "127.0.0.1:7547",
		"--cr-path", "/connreq",
		"--cr-publish-path", "InternetGatewayDevice.ManagementServer.ConnectionRequestURL",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CRBindAddr != "127.0.0.1:7547" {
		t.Errorf("CRBindAddr = %q", cfg.CRBindAddr)
	}
	if cfg.CRPath != "/connreq" {
		t.Errorf("CRPath = %q", cfg.CRPath)
	}
	if cfg.CRPublishPath != "InternetGatewayDevice.ManagementServer.ConnectionRequestURL" {
		t.Errorf("CRPublishPath = %q", cfg.CRPublishPath)
	}
}

func TestLoadCREnvOverride(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load(nil, map[string]string{
		"CPE_SIM_CR_BIND_ADDR":    "127.0.0.1:9000",
		"CPE_SIM_CR_PATH":         "/x",
		"CPE_SIM_CR_PUBLISH_PATH": "Device.X.URL",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CRBindAddr != "127.0.0.1:9000" {
		t.Errorf("CRBindAddr = %q", cfg.CRBindAddr)
	}
	if cfg.CRPath != "/x" {
		t.Errorf("CRPath = %q", cfg.CRPath)
	}
	if cfg.CRPublishPath != "Device.X.URL" {
		t.Errorf("CRPublishPath = %q", cfg.CRPublishPath)
	}
}

func TestLoadCRYAMLLoad(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, `
crBindAddr: 127.0.0.1:7547
crPath: /cr2
crPublishPath: InternetGatewayDevice.ManagementServer.ConnectionRequestURL
`)
	cfg, err := cpeconfig.Load([]string{"--config", path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CRBindAddr != "127.0.0.1:7547" {
		t.Errorf("CRBindAddr = %q", cfg.CRBindAddr)
	}
	if cfg.CRPath != "/cr2" {
		t.Errorf("CRPath = %q", cfg.CRPath)
	}
}

func TestLoadCRPathRequiresLeadingSlash(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{
		"--cr-bind-addr", "127.0.0.1:7547",
		"--cr-path", "no-slash",
	}, nil)
	if err == nil {
		t.Fatal("expected error for cr-path missing leading /")
	}
	if !strings.Contains(err.Error(), "cr-path") {
		t.Errorf("error = %q, expected mention of cr-path", err.Error())
	}
}

func TestLoadCRPathValidationSkippedWhenBindAddrEmpty(t *testing.T) {
	t.Parallel()

	// CRBindAddr empty -> CRPath garbage is fine (validation skipped).
	cfg, err := cpeconfig.Load([]string{
		"--cr-path", "garbage",
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CRPath != "garbage" {
		t.Errorf("CRPath = %q", cfg.CRPath)
	}
}

// writeYAML writes a YAML document to a tempfile in t.TempDir() and
// returns the path. Leading whitespace is trimmed from each line so
// callers can use indented heredocs.
func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func TestLoadFleetOffsetUnsetIsNil(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load(nil, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Nil, not 0: the profile's fleet.offset only gets a say when no
	// higher-precedence source spoke, and an explicit --fleet-offset=0
	// has to be able to override a profile that declares one.
	if cfg.FleetOffset != nil {
		t.Errorf("FleetOffset = %d, want nil", *cfg.FleetOffset)
	}
}

func TestLoadFleetOffsetPrecedence(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, `
		fleetOffset: 100
	`)
	env := map[string]string{
		"CPE_SIM_CONFIG":       path,
		"CPE_SIM_FLEET_OFFSET": "200",
	}

	cfg, err := cpeconfig.Load(nil, env)
	if err != nil {
		t.Fatalf("Load (env over file): %v", err)
	}
	if cfg.FleetOffset == nil || *cfg.FleetOffset != 200 {
		t.Errorf("env should beat file: %v", cfg.FleetOffset)
	}

	cfg, err = cpeconfig.Load([]string{"--fleet-offset=300"}, env)
	if err != nil {
		t.Fatalf("Load (flag over env): %v", err)
	}
	if cfg.FleetOffset == nil || *cfg.FleetOffset != 300 {
		t.Errorf("flag should beat env: %v", cfg.FleetOffset)
	}

	cfg, err = cpeconfig.Load([]string{"--config=" + path}, nil)
	if err != nil {
		t.Fatalf("Load (file only): %v", err)
	}
	if cfg.FleetOffset == nil || *cfg.FleetOffset != 100 {
		t.Errorf("file value not read: %v", cfg.FleetOffset)
	}
}

func TestLoadFleetOffsetExplicitZeroIsSet(t *testing.T) {
	t.Parallel()

	cfg, err := cpeconfig.Load([]string{"--fleet-offset=0"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FleetOffset == nil {
		t.Fatal("--fleet-offset=0 must record a set value, not nil")
	}
	if *cfg.FleetOffset != 0 {
		t.Errorf("FleetOffset = %d, want 0", *cfg.FleetOffset)
	}
}

func TestLoadFleetOffsetNegativeRejected(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load([]string{"--fleet-offset=-1"}, nil)
	if err == nil {
		t.Fatal("negative fleet-offset must reject")
	}
	if !strings.Contains(err.Error(), "fleet-offset") {
		t.Errorf("error should name the flag: %v", err)
	}
}

func TestLoadFleetOffsetInvalidEnv(t *testing.T) {
	t.Parallel()

	_, err := cpeconfig.Load(nil, map[string]string{"CPE_SIM_FLEET_OFFSET": "many"})
	if err == nil {
		t.Fatal("unparseable CPE_SIM_FLEET_OFFSET must reject")
	}
}
