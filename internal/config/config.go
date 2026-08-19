// Package config resolves one run's configuration from flags, the environment
// and .env files.
//
// # One table, not three places
//
// Version 1 of this tool needed a new configuration field written in three
// places — the struct, a hand-written block of viper lookups, and the example
// file — and missing any of them left the field silently at its zero value.
// Here a field is one row of [Specs], and the flag, the environment key, the
// default, the help line and the row printed by `migrator config` are all
// derived from it. TestSpecsMatchConfig then checks by reflection that the
// table and the struct have not drifted apart, which is a machine doing the job
// a convention used to fail at.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/efureev/envi/v2"
	"github.com/efureev/envi/v2/bind"
)

// A Kind says how a setting's text is read.
type Kind uint8

// The kinds of setting.
const (
	KindString Kind = iota
	KindBool
	KindInt
	KindDuration
)

// A Spec describes one setting once.
type Spec struct {
	// Flag is the long flag name, without dashes.
	Flag string
	// Short is the one-letter alias, or "".
	Short string
	// Key is the environment variable.
	Key string
	// Kind says how the value is parsed.
	Kind Kind
	// Default is the value used when nothing else supplies one.
	Default string
	// Help is the one-line description shown by --help.
	Help string
	// Secret marks a value that must never be printed.
	Secret bool
	// Global marks a setting accepted before or after the command name.
	Global bool
}

// Specs is every setting this program has.
//
// Adding a setting means adding a row here and a field to [Config] with the
// matching env tag. Nothing else.
var Specs = []Spec{
	{Flag: "dsn", Key: "MIGRATOR_DSN", Kind: KindString, Secret: true, Global: true,
		Help: "PostgreSQL connection string; empty reads PGHOST, PGUSER and the rest, like psql"},
	{Flag: "dir", Short: "d", Key: "MIGRATOR_DIR", Kind: KindString, Default: "./migrations", Global: true,
		Help: "directory holding the migration files"},
	{Flag: "schema", Key: "MIGRATOR_SCHEMA", Kind: KindString, Default: "public", Global: true,
		Help: "schema holding the journal"},
	{Flag: "table", Key: "MIGRATOR_TABLE", Kind: KindString, Default: "schema_migrations", Global: true,
		Help: "journal table name"},
	{Flag: "env", Key: "MIGRATOR_ENV", Kind: KindString, Global: true,
		Help: "development, staging or production; inferred when unset"},
	{Flag: "env-file", Key: "MIGRATOR_ENV_FILE", Kind: KindString, Global: true,
		Help: "comma-separated .env files to read; .env is read when it exists"},
	{Flag: "timeout", Key: "MIGRATOR_TIMEOUT", Kind: KindDuration, Default: "0", Global: true,
		Help: "limit on the whole command; 0 means none"},
	{Flag: "advisory-lock-timeout", Key: "MIGRATOR_ADVISORY_LOCK_TIMEOUT", Kind: KindDuration, Default: "30s", Global: true,
		Help: "how long to wait for another migrator to finish"},
	{Flag: "lock-timeout", Key: "MIGRATOR_LOCK_TIMEOUT", Kind: KindDuration, Default: "3s", Global: true,
		Help: "PostgreSQL lock_timeout for the run; 0 disables it"},
	{Flag: "statement-timeout", Key: "MIGRATOR_STATEMENT_TIMEOUT", Kind: KindDuration, Default: "0", Global: true,
		Help: "PostgreSQL statement_timeout for the run; 0 means none"},
	{Flag: "log-level", Key: "MIGRATOR_LOG_LEVEL", Kind: KindString, Default: "info", Global: true,
		Help: "debug, info, warn or error"},
	{Flag: "json", Key: "MIGRATOR_JSON", Kind: KindBool, Default: "false", Global: true,
		Help: "write machine-readable output"},
	{Flag: "no-color", Key: "MIGRATOR_NO_COLOR", Kind: KindBool, Default: "false", Global: true,
		Help: "never emit colour, whatever the terminal says"},
	{Flag: "quiet", Short: "q", Key: "", Kind: KindBool, Default: "false", Global: true,
		Help: "print only the answer, not the commentary"},
	{Flag: "verbose", Key: "", Kind: KindBool, Default: "false", Global: true,
		Help: "print debug commentary; the same as --log-level=debug"},
	{Flag: "allow-out-of-order", Key: "MIGRATOR_ALLOW_OUT_OF_ORDER", Kind: KindBool, Default: "false", Global: true,
		Help: "apply a pending migration older than the current version"},
}

// A Config is one run's resolved configuration.
//
// Defaults live in the tags and are applied by bind. The provenance of each
// value — flag, environment, file or default — is tracked separately, because
// `migrator config` has to answer "where did this come from", which is the
// question people actually have.
type Config struct {
	DSN    string `env:"MIGRATOR_DSN"`
	Dir    string `env:"MIGRATOR_DIR,default=./migrations"`
	Schema string `env:"MIGRATOR_SCHEMA,default=public"`
	Table  string `env:"MIGRATOR_TABLE,default=schema_migrations"`
	Env    string `env:"MIGRATOR_ENV"`

	EnvFile string `env:"MIGRATOR_ENV_FILE"`

	Timeout             time.Duration `env:"MIGRATOR_TIMEOUT,default=0"`
	AdvisoryLockTimeout time.Duration `env:"MIGRATOR_ADVISORY_LOCK_TIMEOUT,default=30s"`
	LockTimeout         time.Duration `env:"MIGRATOR_LOCK_TIMEOUT,default=3s"`
	StatementTimeout    time.Duration `env:"MIGRATOR_STATEMENT_TIMEOUT,default=0"`

	LogLevel string `env:"MIGRATOR_LOG_LEVEL,default=info"`
	JSON     bool   `env:"MIGRATOR_JSON,default=false"`
	NoColor  bool   `env:"MIGRATOR_NO_COLOR,default=false"`
	Quiet    bool   `env:"-"`
	Verbose  bool   `env:"-"`

	AllowOutOfOrder bool `env:"MIGRATOR_ALLOW_OUT_OF_ORDER,default=false"`

	// origins records where each setting came from, keyed by flag name.
	origins map[string]string
}

// Origin reports where a setting's value came from.
func (c *Config) Origin(flag string) string {
	if c.origins == nil {
		return "default"
	}

	if o, ok := c.origins[flag]; ok {
		return o
	}

	return "default"
}

// setOrigin records where a value came from.
func (c *Config) setOrigin(flag, origin string) {
	if c.origins == nil {
		c.origins = map[string]string{}
	}

	c.origins[flag] = origin
}

// Load resolves the configuration.
//
// The order, highest first: a flag actually typed, then MIGRATOR_*, then the
// compatibility keys (DATABASE_URL and libpq's PG*), then the .env files named
// by --env-file, then ./.env if it exists, then the tag default.
//
// envFiles and flagValues come from the command layer: flagValues holds only
// the flags that were actually set, which is what makes "the flag wins" mean
// "the flag that was typed wins" rather than "the flag's zero value wins".
func Load(envFiles []string, flagValues map[string]string) (*Config, error) {
	cfg := &Config{}

	// Files first, lowest priority of the explicit sources. envi merges them in
	// order, later winning, and the process environment beats all of them.
	files := envFiles
	if len(files) == 0 {
		if _, err := os.Stat(".env"); err == nil {
			files = []string{".env"}
		}
	}

	opts := []bind.Option{bind.WithEnviron()}
	if len(files) > 0 {
		opts = append(opts, bind.WithOptionalFiles(files...))
	}

	if err := bind.Load(cfg, opts...); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	for _, spec := range Specs {
		if spec.Key != "" {
			if _, ok := os.LookupEnv(spec.Key); ok {
				cfg.setOrigin(spec.Flag, "env "+spec.Key)
			}
		}
	}

	if len(files) > 0 {
		if err := markFileOrigins(cfg, files); err != nil {
			return nil, err
		}
	}

	// Compatibility, below MIGRATOR_* and above the defaults.
	if cfg.DSN == "" {
		if v := os.Getenv("DATABASE_URL"); v != "" {
			cfg.DSN = v
			cfg.setOrigin("dsn", "env DATABASE_URL")
		}
	}

	if err := applyFlags(cfg, flagValues); err != nil {
		return nil, err
	}

	err := cfg.Validate()

	return cfg, err
}

// markFileOrigins records which settings a .env file supplied.
func markFileOrigins(cfg *Config, files []string) error {
	for _, name := range files {
		env, err := envi.Load(name)
		if err != nil {
			// A missing optional file is not a problem; a malformed one is.
			if os.IsNotExist(err) {
				continue
			}

			return fmt.Errorf("configuration: read %s: %w", name, err)
		}

		for _, spec := range Specs {
			if spec.Key == "" {
				continue
			}

			if row := env.Get(spec.Key); row != nil {
				if _, fromEnv := os.LookupEnv(spec.Key); !fromEnv {
					cfg.setOrigin(spec.Flag, "file "+name)
				}
			}
		}
	}

	return nil
}

// applyFlags overlays the flags that were actually typed.
func applyFlags(cfg *Config, values map[string]string) error {
	var errs []error

	for flag, raw := range values {
		spec, ok := specFor(flag)
		if !ok {
			errs = append(errs, fmt.Errorf("unknown setting %q", flag))

			continue
		}

		if err := assign(cfg, spec, raw); err != nil {
			errs = append(errs, err)

			continue
		}

		cfg.setOrigin(flag, "flag --"+flag)
	}

	return errors.Join(errs...)
}

// specFor reports the spec with the given flag name.
func specFor(flag string) (Spec, bool) {
	for _, s := range Specs {
		if s.Flag == flag {
			return s, true
		}
	}

	return Spec{}, false
}

// assign writes one raw value into the config.
func assign(cfg *Config, spec Spec, raw string) error {
	switch spec.Flag {
	case "dsn":
		cfg.DSN = raw
	case "dir":
		cfg.Dir = raw
	case "schema":
		cfg.Schema = raw
	case "table":
		cfg.Table = raw
	case "env":
		cfg.Env = raw
	case "env-file":
		cfg.EnvFile = raw
	case "log-level":
		cfg.LogLevel = raw
	case "timeout":
		return assignDuration(&cfg.Timeout, spec, raw)
	case "advisory-lock-timeout":
		return assignDuration(&cfg.AdvisoryLockTimeout, spec, raw)
	case "lock-timeout":
		return assignDuration(&cfg.LockTimeout, spec, raw)
	case "statement-timeout":
		return assignDuration(&cfg.StatementTimeout, spec, raw)
	case "json":
		return assignBool(&cfg.JSON, spec, raw)
	case "no-color":
		return assignBool(&cfg.NoColor, spec, raw)
	case "quiet":
		return assignBool(&cfg.Quiet, spec, raw)
	case "verbose":
		return assignBool(&cfg.Verbose, spec, raw)
	case "allow-out-of-order":
		return assignBool(&cfg.AllowOutOfOrder, spec, raw)
	default:
		return fmt.Errorf("unknown setting %q", spec.Flag)
	}

	return nil
}

func assignDuration(dst *time.Duration, spec Spec, raw string) error {
	// A bare number is seconds, which is what people type; anything else goes
	// through the usual parser.
	if n, err := strconv.Atoi(raw); err == nil {
		*dst = time.Duration(n) * time.Second

		return nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("--%s: %q is not a duration: %w", spec.Flag, raw, err)
	}

	*dst = d

	return nil
}

func assignBool(dst *bool, spec Spec, raw string) error {
	if raw == "" {
		*dst = true

		return nil
	}

	b, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("--%s: %q is not a boolean", spec.Flag, raw)
	}

	*dst = b

	return nil
}

// Validate reports every problem with the configuration at once.
//
// All of them, not the first: a misconfigured deployment fixed one restart at a
// time is the same waste as a drifted schema fixed one migration at a time.
func (c *Config) Validate() error {
	var errs []error

	if !identifierOK(c.Schema) {
		errs = append(errs, fmt.Errorf("--schema: %q is not a valid unquoted identifier", c.Schema))
	}

	if !identifierOK(c.Table) {
		errs = append(errs, fmt.Errorf("--table: %q is not a valid unquoted identifier", c.Table))
	}

	if c.Env != "" && !envOK(c.Env) {
		errs = append(errs, fmt.Errorf("--env: %q is not development, staging or production", c.Env))
	}

	if _, err := parseLogLevel(c.LogLevel); err != nil {
		errs = append(errs, fmt.Errorf("--log-level: %w", err))
	}

	if c.Quiet && c.Verbose {
		errs = append(errs, errors.New("--quiet and --verbose contradict each other"))
	}

	for name, d := range map[string]time.Duration{
		"--timeout": c.Timeout, "--advisory-lock-timeout": c.AdvisoryLockTimeout,
		"--lock-timeout": c.LockTimeout, "--statement-timeout": c.StatementTimeout,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s: %s is negative", name, d))
		}
	}

	return errors.Join(errs...)
}

// EnvFiles reports the .env files named on the command line.
func (c *Config) EnvFiles() []string {
	if c.EnvFile == "" {
		return nil
	}

	var out []string

	for part := range strings.SplitSeq(c.EnvFile, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// RedactedDSN reports the connection string with the password removed.
//
// The password is taken out by rebuilding a parsed URL, never by a regular
// expression over the original text. If the string does not parse, nothing of
// it is printed at all: the usual reason a DSN fails to parse is a password
// holding a character that needed escaping, and echoing it "for debugging"
// leaks exactly the secret being hidden.
func RedactedDSN(dsn string) string {
	if dsn == "" {
		return ""
	}

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "<unparseable, hidden>"
		}

		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				u.User = url.User(u.User.Username())
			}
		}

		return u.String()
	}

	// keyword/value form.
	var out []string

	for _, field := range strings.Fields(dsn) {
		if strings.HasPrefix(field, "password=") {
			out = append(out, "password=xxxxx")

			continue
		}

		out = append(out, field)
	}

	return strings.Join(out, " ")
}

func identifierOK(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}

	for i := range len(s) {
		c := s[i]

		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case (c >= '0' && c <= '9') && i > 0:
		default:
			return false
		}
	}

	return true
}

func envOK(s string) bool {
	switch strings.ToLower(s) {
	case "development", "dev", "test", "local", "staging", "stage", "production", "prod", "live":
		return true
	default:
		return false
	}
}

// parseLogLevel is the same parser ui.ParseLevel uses; duplicated as a check
// only, so that config does not import the presentation layer.
func parseLogLevel(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "info", "warn", "warning", "error", "":
		return s, nil
	default:
		return "", fmt.Errorf("%q is not debug, info, warn or error", s)
	}
}
