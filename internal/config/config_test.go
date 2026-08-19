package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestSpecsMatchConfig is the machine doing the job a convention used to fail
// at.
//
// Version 1 needed a new configuration field written in three places, and
// missing one left the field silently at its zero value. Here there are two —
// the Specs table and the struct tag — and this test refuses to let them drift.
func TestSpecsMatchConfig(t *testing.T) {
	t.Parallel()

	tagged := map[string]string{} // env key -> field name

	rt := reflect.TypeFor[Config]()

	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}

		tag := f.Tag.Get("env")
		if tag == "" || tag == "-" {
			continue
		}

		key, _, _ := strings.Cut(tag, ",")
		tagged[key] = f.Name
	}

	specKeys := map[string]bool{}

	for _, s := range Specs {
		if s.Key == "" {
			// A setting with no environment key is flag-only, deliberately:
			// --quiet and --verbose are about one invocation, and an inherited
			// variable that silences a tool is a bad surprise.
			continue
		}

		specKeys[s.Key] = true

		if _, ok := tagged[s.Key]; !ok {
			t.Errorf("Specs has %q (--%s) but no Config field carries that env tag", s.Key, s.Flag)
		}
	}

	for key, field := range tagged {
		if !specKeys[key] {
			t.Errorf("Config.%s is tagged %q but no row of Specs describes it", field, key)
		}
	}
}

// TestSpecDefaultsMatchTags: the default a user reads in --help must be the
// default the code applies.
func TestSpecDefaultsMatchTags(t *testing.T) {
	t.Parallel()

	rt := reflect.TypeFor[Config]()
	byKey := map[string]string{} // env key -> default from the tag

	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("env")
		if tag == "" || tag == "-" {
			continue
		}

		key, rest, _ := strings.Cut(tag, ",")

		var def string
		for part := range strings.SplitSeq(rest, ",") {
			if after, ok := strings.CutPrefix(part, "default="); ok {
				def = after
			}
		}

		byKey[key] = def
	}

	for _, s := range Specs {
		if s.Key == "" {
			continue
		}

		if got := byKey[s.Key]; got != s.Default {
			t.Errorf("--%s: Specs says the default is %q, the struct tag says %q", s.Flag, s.Default, got)
		}
	}
}

func TestSpecsAreWellFormed(t *testing.T) {
	t.Parallel()

	seenFlag := map[string]bool{}
	seenShort := map[string]bool{}

	for _, s := range Specs {
		if s.Flag == "" {
			t.Error("a spec has no flag name")
		}

		if seenFlag[s.Flag] {
			t.Errorf("--%s is declared twice", s.Flag)
		}

		seenFlag[s.Flag] = true

		if s.Short != "" {
			if seenShort[s.Short] {
				t.Errorf("-%s is declared twice", s.Short)
			}

			seenShort[s.Short] = true
		}

		if s.Help == "" {
			t.Errorf("--%s has no help line, so --help would show a blank column", s.Flag)
		}
	}
}

func TestRedactedDSN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		want   string
		hidden string
	}{
		{
			name: "url",
			in:   "postgres://app:s3cr3t@db:5432/shop?sslmode=require",
			want: "postgres://app@db:5432/shop?sslmode=require",
		},
		{
			name: "url without a password",
			in:   "postgres://app@db:5432/shop",
			want: "postgres://app@db:5432/shop",
		},
		{
			name: "keyword form",
			in:   "host=db user=app password=s3cr3t dbname=shop",
			want: "host=db user=app password=xxxxx dbname=shop",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			// url.Parse splits userinfo at the last @, so a password holding one
			// still parses and is still removed.
			name: "password holding an at sign",
			in:   "postgres://app:p@ssword@db:5432/shop",
			want: "postgres://app@db:5432/shop",
		},
		{
			// When it genuinely does not parse, nothing of it is printed. The
			// usual reason is a password holding a character that needed
			// escaping, and echoing it "for debugging" leaks exactly the secret
			// being hidden.
			name:   "unparseable",
			in:     "postgres://app:s3cr3t@db:notaport/shop",
			want:   "<unparseable, hidden>",
			hidden: "s3cr3t",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := RedactedDSN(tc.in)
			if got != tc.want {
				t.Errorf("RedactedDSN(%q) = %q, want %q", tc.in, got, tc.want)
			}

			if tc.hidden != "" && strings.Contains(got, tc.hidden) {
				t.Errorf("RedactedDSN(%q) still holds the secret", tc.in)
			}

			if strings.Contains(got, "s3cr3t") {
				t.Errorf("RedactedDSN(%q) = %q, which still holds the password", tc.in, got)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	ok := &Config{Schema: "public", Table: "schema_migrations", LogLevel: "info"}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid configuration was rejected: %v", err)
	}

	bad := &Config{
		Schema: "Public", Table: "a-b", Env: "someday", LogLevel: "loud",
		Quiet: true, Verbose: true, Timeout: -time.Second,
	}

	err := bad.Validate()
	if err == nil {
		t.Fatal("an invalid configuration was accepted")
	}

	// Every problem at once: fixing a misconfigured deployment one restart at a
	// time is the same waste as fixing a drifted schema one migration at a time.
	for _, want := range []string{"--schema", "--table", "--env", "--log-level", "--quiet", "--timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate did not report %s:\n%v", want, err)
		}
	}
}

func TestEnvFiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{".env", []string{".env"}},
		{".env,.env.local", []string{".env", ".env.local"}},
		{" .env , .env.local , ", []string{".env", ".env.local"}},
	}

	for _, tc := range cases {
		in, want := tc.in, tc.want
		got := (&Config{EnvFile: in}).EnvFiles()
		if len(got) != len(want) {
			t.Errorf("EnvFiles(%q) = %v, want %v", in, got, want)

			continue
		}

		for i := range want {
			if got[i] != want[i] {
				t.Errorf("EnvFiles(%q) = %v, want %v", in, got, want)
			}
		}
	}
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()

	t.Chdir(dir)
	t.Setenv("MIGRATOR_SCHEMA", "from_env")
	t.Setenv("MIGRATOR_TABLE", "from_env_table")

	// A flag actually typed beats the environment; an environment variable
	// beats the tag default; a setting nobody mentioned takes the default.
	cfg, err := Load(nil, map[string]string{"schema": "from_flag"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Schema != "from_flag" {
		t.Errorf("Schema = %q, want the flag to win", cfg.Schema)
	}

	if cfg.Table != "from_env_table" {
		t.Errorf("Table = %q, want the environment to win over the default", cfg.Table)
	}

	if cfg.Dir != "./migrations" {
		t.Errorf("Dir = %q, want the default", cfg.Dir)
	}

	// The provenance is what `migrator config` prints, and it is the question
	// people actually have when a value is not what they expected.
	if got := cfg.Origin("schema"); got != "flag --schema" {
		t.Errorf("Origin(schema) = %q", got)
	}

	if got := cfg.Origin("table"); got != "env MIGRATOR_TABLE" {
		t.Errorf("Origin(table) = %q", got)
	}

	if got := cfg.Origin("dir"); got != "default" {
		t.Errorf("Origin(dir) = %q", got)
	}
}

func TestLoadReadsDatabaseURL(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://app@db/shop")

	cfg, err := Load(nil, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.DSN != "postgres://app@db/shop" {
		t.Errorf("DSN = %q, want DATABASE_URL to be read", cfg.DSN)
	}

	if got := cfg.Origin("dsn"); got != "env DATABASE_URL" {
		t.Errorf("Origin(dsn) = %q", got)
	}

	// MIGRATOR_DSN outranks it.
	t.Setenv("MIGRATOR_DSN", "postgres://app@other/shop")

	cfg, err = Load(nil, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.DSN != "postgres://app@other/shop" {
		t.Errorf("DSN = %q, want MIGRATOR_DSN to win over DATABASE_URL", cfg.DSN)
	}
}

func TestLoadRejectsBadFlagValues(t *testing.T) {
	t.Chdir(t.TempDir())

	for flag, value := range map[string]string{
		"timeout": "soon",
		"json":    "perhaps",
	} {
		if _, err := Load(nil, map[string]string{flag: value}); err == nil {
			t.Errorf("Load accepted --%s=%s", flag, value)
		}
	}

	if _, err := Load(nil, map[string]string{"nonesuch": "1"}); err == nil {
		t.Error("Load accepted an unknown setting")
	}

	// A bare number is seconds, which is what people type.
	cfg, err := Load(nil, map[string]string{"timeout": "90"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Timeout != 90*time.Second {
		t.Errorf("--timeout=90 became %v, want 90s", cfg.Timeout)
	}
}
