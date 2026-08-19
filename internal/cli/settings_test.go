package cli

import (
	"testing"

	"github.com/efureev/db-migrator/v2/internal/config"
)

// sampleFor gives every setting a value a person might type.
//
// A new row in config.Specs with no entry here fails the test below on purpose:
// whoever adds a setting is the one who knows what a valid value for it looks
// like, and that is a cheaper moment to say so than the first bug report.
var sampleFor = map[string]string{
	"dsn":                   "postgres://u@h/db",
	"dir":                   "./migrations",
	"schema":                "app",
	"table":                 "schema_migrations",
	"env":                   "development",
	"env-file":              ".env.test",
	"timeout":               "5m",
	"advisory-lock-timeout": "45s",
	"lock-timeout":          "5s",
	"statement-timeout":     "1h",
	"log-level":             "debug",
	"json":                  "true",
	"no-color":              "true",
	"quiet":                 "true",
	"verbose":               "false",
	"allow-out-of-order":    "true",
	"max-lock-level":        "share-update-exclusive",
	"progress-interval":     "45s",
}

// TestEverySettingSurvivesTheRoundTrip closes the two hand-written switches
// that no machine watches today.
//
// config.assign turns a typed flag into a field, and cli.settingValue turns
// that field back into the line `migrator config` prints. Both are a switch on
// the flag name with a silent default, so a setting missing from the first is
// rejected as unknown — exit 2 on a flag the help text advertises — and one
// missing from the second prints an empty column. Neither breaks the build,
// and neither is visible until somebody uses the setting.
//
// TestSpecsMatchConfig next door proves the table and the struct agree. This
// proves the two pieces of code between them agree with both.
func TestEverySettingSurvivesTheRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	for _, spec := range config.Specs {
		raw, ok := sampleFor[spec.Flag]
		if !ok {
			t.Errorf("--%s has no sample value in this test", spec.Flag)

			continue
		}

		cfg, err := config.Load(nil, map[string]string{spec.Flag: raw})
		if err != nil {
			t.Errorf("--%s %s: %v", spec.Flag, raw, err)

			continue
		}

		if got := settingValue(cfg, spec); got == "" {
			t.Errorf("--%s was set to %q and `migrator config` would print nothing for it",
				spec.Flag, raw)
		}
	}
}
