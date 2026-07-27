// Package cpeconfig is the simulator's single source of startup configuration.
//
// Load merges, in deterministic precedence, CLI flags > environment
// variables (prefix CPE_SIM_) > optional YAML file > defaults. Validation
// is strict: unknown YAML keys, unknown CPE_SIM_ env vars, and unknown
// flags all return errors.
package cpeconfig

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

const envPrefix = "CPE_SIM_"

// Config is the simulator's startup configuration. Field names map 1:1
// across all three sources (flag --acs-url ↔ env CPE_SIM_ACS_URL ↔ YAML acsURL).
type Config struct {
	ConfigPath    string        `yaml:"-"` // path the config was loaded from; "" if no file used
	ACSURL        string        `yaml:"acsURL"`
	ACSUsername   string        `yaml:"acsUsername"`
	ACSPassword   string        `yaml:"acsPassword"`
	ACSTimeout    time.Duration `yaml:"acsTimeout"`
	TLSSkipVerify bool          `yaml:"tlsSkipVerify"`
	CACertFile    string        `yaml:"caCertFile"`
	LogLevel      string        `yaml:"logLevel"`
	LogFormat     string        `yaml:"logFormat"`
	Concurrency   int           `yaml:"concurrency"`
	Seed          int64         `yaml:"seed"`
	ProfilePath   string        `yaml:"profile"`
	CRBindAddr    string        `yaml:"crBindAddr"`
	CRPath        string        `yaml:"crPath"`
	CRPublishPath string        `yaml:"crPublishPath"`

	// USP (TR-369) agent. Empty USPBroker leaves the whole USP path off, so a
	// CWMP-only run is unchanged. The controller id and secret mirror what a
	// controller expects: an authority-scheme endpoint id (TR-369 2.2) and the
	// shared secret its MQTT auth derives a per-agent password from.
	USPBroker       string `yaml:"uspBroker"`
	USPControllerID string `yaml:"uspControllerID"`
	USPMQTTSecret   string `yaml:"uspMQTTSecret"`
	USPMQTTUsername string `yaml:"uspMQTTUsername"`
	USPMQTTPassword string `yaml:"uspMQTTPassword"`
}

// defaults returns the baseline Config before any source overlays it.
func defaults() Config {
	return Config{
		LogLevel:    "info",
		LogFormat:   "text",
		Concurrency: 1,
		ACSTimeout:  30 * time.Second,
		CRPath:      "/cr",
		// CRPublishPath has no default. TR-181 / TR-098 / vendor-specific
		// layouts use different paths; baking TR-181 in here would
		// silently cement the model assumption and break the
		// vendor-extensibility premise (design principle #3).
		// Validation requires the operator to supply this when
		// CRBindAddr is non-empty.
	}
}

// knownEnvKeys is the set of CPE_SIM_* env vars Load reads. Any other
// CPE_SIM_* key in the input env returns an error so typos surface loudly.
var knownEnvKeys = map[string]struct{}{
	envPrefix + "CONFIG":            {},
	envPrefix + "ACS_URL":           {},
	envPrefix + "ACS_USERNAME":      {},
	envPrefix + "ACS_PASSWORD":      {},
	envPrefix + "ACS_TIMEOUT":       {},
	envPrefix + "USP_BROKER":        {},
	envPrefix + "USP_CONTROLLER_ID": {},
	envPrefix + "USP_MQTT_SECRET":   {},
	envPrefix + "USP_MQTT_USERNAME": {},
	envPrefix + "USP_MQTT_PASSWORD": {},
	envPrefix + "TLS_SKIP_VERIFY":   {},
	envPrefix + "CA_CERT_FILE":      {},
	envPrefix + "LOG_LEVEL":         {},
	envPrefix + "LOG_FORMAT":        {},
	envPrefix + "CONCURRENCY":       {},
	envPrefix + "SEED":              {},
	envPrefix + "PROFILE":           {},
	envPrefix + "CR_BIND_ADDR":      {},
	envPrefix + "CR_PATH":           {},
	envPrefix + "CR_PUBLISH_PATH":   {},
}

// Load parses configuration from CLI args, env vars, and (optionally) a
// YAML file. Precedence: flags > env > file > defaults.
//
// args should not include argv[0]. env should contain every environment
// variable the caller wants considered; only CPE_SIM_* keys are consumed,
// but every CPE_SIM_* key not in the known set returns an error.
//
// flag.ErrHelp is returned verbatim when args contain --help; callers
// (specifically cmd/cpe-sim) can branch on errors.Is(err, flag.ErrHelp)
// to exit 0.
func Load(args []string, env map[string]string) (Config, error) {
	cfg := defaults()

	// Determine config path from env first, then from a pre-scan of args
	// so we can read the file before the env/flag passes overlay it.
	if v, ok := env[envPrefix+"CONFIG"]; ok && v != "" {
		cfg.ConfigPath = v
	}
	if p, found := scanFlag(args, "config"); found {
		cfg.ConfigPath = p
	}

	if cfg.ConfigPath != "" {
		if err := loadFile(cfg.ConfigPath, &cfg); err != nil {
			return Config{}, cpeerr.Wrap("cpeconfig.Load", cpeerr.KindInvalidArgument, err)
		}
	}

	if err := applyEnv(env, &cfg); err != nil {
		return Config{}, cpeerr.Wrap("cpeconfig.Load", cpeerr.KindInvalidArgument, err)
	}

	if err := applyFlags(args, &cfg); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, err
		}
		return Config{}, cpeerr.Wrap("cpeconfig.Load", cpeerr.KindInvalidArgument, err)
	}

	if err := validate(&cfg); err != nil {
		return Config{}, cpeerr.Wrap("cpeconfig.Load", cpeerr.KindInvalidArgument, err)
	}

	return cfg, nil
}

// EnvMap converts os.Environ()-style "K=V" entries into a map suitable
// for Load. Entries without "=" are skipped. The first "=" splits key
// from value so values containing "=" survive intact.
func EnvMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		m[kv[:i]] = kv[i+1:]
	}
	return m
}

// scanFlag looks for "--name=value", "--name value", "-name=value", or
// "-name value" in args without consuming or validating any other flag.
// It is used to find --config before the full flag pass runs.
func scanFlag(args []string, name string) (string, bool) {
	long := "--" + name
	short := "-" + name
	for i, a := range args {
		switch {
		case a == long || a == short:
			if i+1 < len(args) {
				return args[i+1], true
			}
		case strings.HasPrefix(a, long+"="):
			return strings.TrimPrefix(a, long+"="), true
		case strings.HasPrefix(a, short+"="):
			return strings.TrimPrefix(a, short+"="), true
		}
	}
	return "", false
}

func loadFile(path string, cfg *Config) error {
	f, err := os.Open(path) //nolint:gosec // path comes from operator-supplied config
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode config %s: %w", path, err)
	}
	return nil
}

func applyEnv(env map[string]string, cfg *Config) error {
	for k := range env {
		if !strings.HasPrefix(k, envPrefix) {
			continue
		}
		if _, ok := knownEnvKeys[k]; !ok {
			return fmt.Errorf("unknown env key %q (typo? prefix CPE_SIM_ is reserved for cpe-sim)", k)
		}
	}

	if v, ok := env[envPrefix+"ACS_URL"]; ok && v != "" {
		cfg.ACSURL = v
	}
	if v, ok := env[envPrefix+"ACS_USERNAME"]; ok && v != "" {
		cfg.ACSUsername = v
	}
	if v, ok := env[envPrefix+"ACS_PASSWORD"]; ok && v != "" {
		cfg.ACSPassword = v
	}
	if v, ok := env[envPrefix+"ACS_TIMEOUT"]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("env %sACS_TIMEOUT: %w", envPrefix, err)
		}
		cfg.ACSTimeout = d
	}
	if v, ok := env[envPrefix+"TLS_SKIP_VERIFY"]; ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("env %sTLS_SKIP_VERIFY: %w", envPrefix, err)
		}
		cfg.TLSSkipVerify = b
	}
	if v, ok := env[envPrefix+"CA_CERT_FILE"]; ok && v != "" {
		cfg.CACertFile = v
	}
	if v, ok := env[envPrefix+"LOG_LEVEL"]; ok && v != "" {
		cfg.LogLevel = v
	}
	if v, ok := env[envPrefix+"LOG_FORMAT"]; ok && v != "" {
		cfg.LogFormat = v
	}
	if v, ok := env[envPrefix+"PROFILE"]; ok && v != "" {
		cfg.ProfilePath = v
	}
	if v, ok := env[envPrefix+"CONCURRENCY"]; ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("env %sCONCURRENCY: %w", envPrefix, err)
		}
		cfg.Concurrency = n
	}
	if v, ok := env[envPrefix+"SEED"]; ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("env %sSEED: %w", envPrefix, err)
		}
		cfg.Seed = n
	}
	if v, ok := env[envPrefix+"CR_BIND_ADDR"]; ok && v != "" {
		cfg.CRBindAddr = v
	}
	if v, ok := env[envPrefix+"CR_PATH"]; ok && v != "" {
		cfg.CRPath = v
	}
	if v, ok := env[envPrefix+"CR_PUBLISH_PATH"]; ok && v != "" {
		cfg.CRPublishPath = v
	}
	if v, ok := env[envPrefix+"USP_BROKER"]; ok {
		cfg.USPBroker = v
	}
	if v, ok := env[envPrefix+"USP_CONTROLLER_ID"]; ok {
		cfg.USPControllerID = v
	}
	if v, ok := env[envPrefix+"USP_MQTT_SECRET"]; ok {
		cfg.USPMQTTSecret = v
	}
	if v, ok := env[envPrefix+"USP_MQTT_USERNAME"]; ok {
		cfg.USPMQTTUsername = v
	}
	if v, ok := env[envPrefix+"USP_MQTT_PASSWORD"]; ok {
		cfg.USPMQTTPassword = v
	}
	return nil
}

func applyFlags(args []string, cfg *Config) error {
	fs := flag.NewFlagSet("cpe-sim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr) // standard --help / unknown-flag UX writes here

	// Prime each flag's default with the post-env value so flags only
	// override when the caller explicitly sets them.
	configPath := fs.String("config", cfg.ConfigPath, "path to YAML config file")
	acsURL := fs.String("acs-url", cfg.ACSURL, "ACS endpoint URL")
	acsUsername := fs.String("acs-username", cfg.ACSUsername, "ACS HTTP auth username")
	acsPassword := fs.String("acs-password", cfg.ACSPassword, "ACS HTTP auth password")
	acsTimeout := fs.Duration("acs-timeout", cfg.ACSTimeout, "ACS request timeout (e.g. 30s, 1m)")
	tlsSkipVerify := fs.Bool("tls-skip-verify", cfg.TLSSkipVerify, "disable TLS certificate verification (insecure)")
	caCertFile := fs.String("ca-cert-file", cfg.CACertFile, "path to PEM-encoded CA bundle for TLS verification")
	logLevel := fs.String("log-level", cfg.LogLevel, "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", cfg.LogFormat, "log format: text|json")
	concurrency := fs.Int("concurrency", cfg.Concurrency, "number of simulated CPEs (>= 1)")
	seed := fs.Int64("seed", cfg.Seed, "RNG seed (0 = non-deterministic)")
	profile := fs.String("profile", cfg.ProfilePath, "vendor profile path")
	crBindAddr := fs.String("cr-bind-addr", cfg.CRBindAddr, "TCP address to bind the connection-request listener (empty disables daemon mode)")
	crPath := fs.String("cr-path", cfg.CRPath, "URL path the connection-request listener serves")
	crPublishPath := fs.String("cr-publish-path", cfg.CRPublishPath, "parameter-tree path where the listener URL is published")
	uspBroker := fs.String("usp-broker", cfg.USPBroker, "USP MQTT broker host:port (enables the TR-369 agent)")
	uspControllerID := fs.String("usp-controller-id", cfg.USPControllerID, "USP controller endpoint id, e.g. self::controller")
	uspMQTTSecret := fs.String("usp-mqtt-secret", cfg.USPMQTTSecret, "shared secret the MQTT password is derived from")
	uspMQTTUsername := fs.String("usp-mqtt-username", cfg.USPMQTTUsername, "USP MQTT username (default: the agent endpoint id)")
	uspMQTTPassword := fs.String("usp-mqtt-password", cfg.USPMQTTPassword, "USP MQTT password (overrides --usp-mqtt-secret)")
	// --version is documented here but consumed by main() before Load runs;
	// keeping it in the FlagSet means --help lists it and it does not error
	// out as "unknown flag" if it appears alongside other flags.
	_ = fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument: %q", fs.Arg(0))
	}

	cfg.ConfigPath = *configPath
	cfg.ACSURL = *acsURL
	cfg.ACSUsername = *acsUsername
	cfg.ACSPassword = *acsPassword
	cfg.ACSTimeout = *acsTimeout
	cfg.TLSSkipVerify = *tlsSkipVerify
	cfg.CACertFile = *caCertFile
	cfg.LogLevel = *logLevel
	cfg.LogFormat = *logFormat
	cfg.Concurrency = *concurrency
	cfg.Seed = *seed
	cfg.ProfilePath = *profile
	cfg.CRBindAddr = *crBindAddr
	cfg.CRPath = *crPath
	cfg.CRPublishPath = *crPublishPath
	cfg.USPBroker = *uspBroker
	cfg.USPControllerID = *uspControllerID
	cfg.USPMQTTSecret = *uspMQTTSecret
	cfg.USPMQTTUsername = *uspMQTTUsername
	cfg.USPMQTTPassword = *uspMQTTPassword
	return nil
}

func validate(cfg *Config) error {
	switch strings.ToLower(cfg.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("invalid log-level %q (want one of debug|info|warn|error)", cfg.LogLevel)
	}
	switch strings.ToLower(cfg.LogFormat) {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log-format %q (want \"text\" or \"json\")", cfg.LogFormat)
	}
	if cfg.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1, got %d", cfg.Concurrency)
	}
	if cfg.ACSTimeout <= 0 {
		return fmt.Errorf("acs-timeout must be > 0, got %s", cfg.ACSTimeout)
	}
	if cfg.CRBindAddr != "" {
		if !strings.HasPrefix(cfg.CRPath, "/") {
			return fmt.Errorf("cr-path must start with %q, got %q", "/", cfg.CRPath)
		}
		if cfg.CRPublishPath == "" {
			return fmt.Errorf("cr-publish-path is required when cr-bind-addr is set " +
				"(no default; supply the parameter-tree path the listener URL should be written to, " +
				"e.g. Device.ManagementServer.ConnectionRequestURL for TR-181 or " +
				"InternetGatewayDevice.ManagementServer.ConnectionRequestURL for TR-098)")
		}
	}
	return nil
}
