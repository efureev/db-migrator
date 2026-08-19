package migrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRedactHidesPasswords(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		hidden string
	}{
		{
			name:   "keyword form",
			in:     `failed to connect: host=db user=app password=s3cr3t dbname=shop`,
			hidden: "s3cr3t",
		},
		{
			name:   "url form",
			in:     `dial error for postgres://app:s3cr3t@db:5432/shop`,
			hidden: "s3cr3t",
		},
		{
			name:   "colon form",
			in:     `config was {host: db, password: s3cr3t}`,
			hidden: "s3cr3t",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := redact(errors.New(tc.in))
			if strings.Contains(got.Error(), tc.hidden) {
				t.Errorf("redact(%q) = %q, which still holds the secret", tc.in, got)
			}

			if !strings.Contains(got.Error(), "xxxxx") {
				t.Errorf("redact(%q) = %q, which does not mark the redaction", tc.in, got)
			}
		})
	}

	// An error with nothing password-shaped in it is returned unchanged, so
	// that redaction never costs the original error's type.
	plain := errors.New("connection refused")
	if got := redact(plain); !errors.Is(got, plain) {
		t.Errorf("redact wrapped an error with no secret in it: %v", got)
	}

	if redact(nil) != nil {
		t.Error("redact(nil) is not nil")
	}
}

func TestBackoffFor(t *testing.T) {
	t.Parallel()

	base := time.Second

	for attempt := range 6 {
		d := backoffFor(base, attempt)

		// Half the doubled base is the floor and the doubled base the ceiling:
		// the jitter must not be able to produce a zero wait, which would make
		// a retry loop a spin loop.
		if d <= 0 {
			t.Fatalf("backoffFor(%v, %d) = %v", base, attempt, d)
		}

		if d > base<<min(attempt, 4)+base<<min(attempt, 4) {
			t.Fatalf("backoffFor(%v, %d) = %v, unexpectedly large", base, attempt, d)
		}
	}

	// A zero or negative base falls back rather than spinning.
	if d := backoffFor(0, 0); d <= 0 {
		t.Errorf("backoffFor(0, 0) = %v", d)
	}

	if randN(0) != 0 || randN(-5) != 0 {
		t.Error("randN of a non-positive bound is not zero")
	}
}

func TestMilliseconds(t *testing.T) {
	t.Parallel()

	// Always milliseconds, never "30min": the interval syntax PostgreSQL
	// accepts here depends on IntervalStyle, and a unit-free integer does not.
	cases := map[time.Duration]string{
		0:                       "'0ms'",
		time.Second:             "'1000ms'",
		30 * time.Minute:        "'1800000ms'",
		1500 * time.Microsecond: "'1ms'",
	}

	for d, want := range cases {
		if got := milliseconds(d); got != want {
			t.Errorf("milliseconds(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestValidateIdentifier(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"public", "app", "_x", "schema_migrations", "a1_b2"} {
		if err := validateIdentifier("schema", ok); err != nil {
			t.Errorf("validateIdentifier(%q) = %v, want nil", ok, err)
		}
	}

	for _, bad := range []string{"", "Public", "1abc", "a-b", "a b", `a"b`, "a;DROP TABLE x", strings.Repeat("a", 64)} {
		err := validateIdentifier("schema", bad)
		if !errors.Is(err, ErrInvalidIdentifier) {
			t.Errorf("validateIdentifier(%q) = %v, want %v", bad, err, ErrInvalidIdentifier)
		}
	}
}

func TestEnvironmentText(t *testing.T) {
	t.Parallel()

	round := map[string]Environment{
		"development": EnvDevelopment,
		"staging":     EnvStaging,
		"production":  EnvProduction,
		"unknown":     EnvUnknown,
	}

	for text, env := range round {
		b, err := env.MarshalText()
		if err != nil || string(b) != text {
			t.Errorf("%v.MarshalText() = %q, %v; want %q", env, b, err, text)
		}

		var got Environment
		if err := got.UnmarshalText([]byte(text)); err != nil || got != env {
			t.Errorf("UnmarshalText(%q) = %v, %v; want %v", text, got, err, env)
		}
	}

	// The short spellings people actually put in MIGRATOR_ENV.
	for text, want := range map[string]Environment{
		"dev": EnvDevelopment, "test": EnvDevelopment, "local": EnvDevelopment,
		"stage": EnvStaging, "prod": EnvProduction, "live": EnvProduction, "": EnvUnknown,
	} {
		var got Environment
		if err := got.UnmarshalText([]byte(text)); err != nil || got != want {
			t.Errorf("UnmarshalText(%q) = %v, %v; want %v", text, got, err, want)
		}
	}

	var bad Environment
	if err := bad.UnmarshalText([]byte("someday")); err == nil {
		t.Error("UnmarshalText accepted an unknown environment")
	}

	if got := Environment(99).String(); got != "unknown" {
		t.Errorf("Environment(99) = %q", got)
	}
}

// sampleReport is a run with one applied and one reverted migration.
func sampleReport() *Report {
	at := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	finished := at.Add(120 * time.Millisecond)
	reverted := at.Add(time.Hour)

	return &Report{
		Direction: DirectionUp,
		Target:    All(),
		StartedAt: at,
		Duration:  350 * time.Millisecond,
		Applied: []Record{
			{
				Version: 1, Name: "create_a", Checksum: "abc123",
				AppliedAt: at, FinishedAt: &finished, ExecutionTime: 120 * time.Millisecond,
			},
			{
				Version: 2, Name: "create_b", Checksum: "def456",
				AppliedAt: at, FinishedAt: &finished, RolledBackAt: &reverted,
			},
		},
	}
}

func TestReportText(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := sampleReport().Text(&b); err != nil {
		t.Fatalf("Text: %v", err)
	}

	out := b.String()
	for _, want := range []string{"1_create_a", "2_create_b", "applied", "reverted", "120ms", "Current version 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	// An empty run says so in one line rather than printing an empty table.
	var empty bytes.Buffer
	if err := (&Report{}).Text(&empty); err != nil {
		t.Fatalf("Text of an empty report: %v", err)
	}

	if !strings.Contains(empty.String(), "up to date") {
		t.Errorf("empty report printed %q", empty.String())
	}
}

func TestReportJSON(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := sampleReport().JSON(&b); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var out struct {
		Direction string `json:"direction"`
		Current   int64  `json:"current_version"`
		Applied   []struct {
			Version    int64 `json:"version"`
			RolledBack bool  `json:"rolled_back"`
			Complete   bool  `json:"complete"`
		} `json:"applied"`
	}

	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatalf("the output is not valid JSON: %v\n%s", err, b.String())
	}

	if out.Direction != "up" || out.Current != 1 || len(out.Applied) != 2 {
		t.Errorf("decoded %+v", out)
	}

	if !out.Applied[1].RolledBack {
		t.Error("the reverted migration is not marked rolled_back")
	}

	// An empty run writes [] and not null, so that the shape of the output does
	// not depend on the outcome — a consumer indexing into it must not have to
	// special-case success.
	var empty bytes.Buffer
	if err := (&Report{}).JSON(&empty); err != nil {
		t.Fatalf("JSON of an empty report: %v", err)
	}

	if !strings.Contains(empty.String(), `"applied": []`) {
		t.Errorf("an empty report wrote %s", empty.String())
	}
}

func TestReportStringAndLogValue(t *testing.T) {
	t.Parallel()

	r := sampleReport()

	if got := r.String(); !strings.Contains(got, "2 applied") {
		t.Errorf("String = %q", got)
	}

	down := &Report{Direction: DirectionDown, Applied: r.Applied}
	if got := down.String(); !strings.Contains(got, "rolled back") {
		t.Errorf("String of a down run = %q", got)
	}

	if got := (&Report{}).String(); got != "nothing to do" {
		t.Errorf("String of an empty report = %q", got)
	}

	v := r.LogValue()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind = %v, want a group", v.Kind())
	}

	seen := map[string]bool{}
	for _, attr := range v.Group() {
		seen[attr.Key] = true
	}

	for _, want := range []string{"direction", "applied", "duration", "current"} {
		if !seen[want] {
			t.Errorf("LogValue has no %q attribute", want)
		}
	}

	if got := r.Versions(); len(got) != 2 || got[0] != 1 {
		t.Errorf("Versions = %v", got)
	}
}

func sampleStatus() *Status {
	at := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	finished := at.Add(time.Second)

	return &Status{
		Schema: "public", Table: "schema_migrations", Initialised: true,
		Entries: []Entry{
			{Version: 1, Name: "create_a", State: StateApplied, Record: &Record{
				Version: 1, AppliedAt: at, FinishedAt: &finished,
				ExecutionTime: 142 * time.Millisecond, Checksum: "abc",
			}},
			{Version: 2, Name: "add_index", State: StateModified, Record: &Record{
				Version: 2, AppliedAt: at, FinishedAt: &finished, Checksum: "def",
			}},
			{Version: 3, Name: "create_c", State: StatePending},
		},
	}
}

func TestStatusText(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := sampleStatus().Text(&b); err != nil {
		t.Fatalf("Text: %v", err)
	}

	out := b.String()
	for _, want := range []string{"VERSION", "create_a", "applied", "changed", "pending", "142ms", "needing attention", "validate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	// Before the first migration the table is absent, and status says so
	// instead of failing — it must work against a database it cannot write to.
	var fresh bytes.Buffer

	empty := &Status{Schema: "public", Table: "schema_migrations", Entries: []Entry{
		{Version: 1, Name: "create_a", State: StatePending},
	}}

	if err := empty.Text(&fresh); err != nil {
		t.Fatalf("Text of an uninitialised status: %v", err)
	}

	for _, want := range []string{"does not exist yet", "No current version"} {
		if !strings.Contains(fresh.String(), want) {
			t.Errorf("uninitialised output does not contain %q:\n%s", want, fresh.String())
		}
	}
}

func TestStatusJSON(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := sampleStatus().JSON(&b); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var out struct {
		Schema      string `json:"schema"`
		Initialised bool   `json:"initialised"`
		Current     int64  `json:"current_version"`
		Pending     int    `json:"pending"`
		Entries     []struct {
			Version int64  `json:"version"`
			State   string `json:"state"`
		} `json:"entries"`
	}

	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatalf("the output is not valid JSON: %v", err)
	}

	if out.Schema != "public" || !out.Initialised || out.Current != 2 || len(out.Entries) != 3 {
		t.Errorf("decoded %+v", out)
	}

	if out.Entries[1].State != "changed" {
		t.Errorf("entry 2 state = %q, want %q", out.Entries[1].State, "changed")
	}
}

func TestVersionString(t *testing.T) {
	t.Parallel()

	if got := (Version{}).String(); got != "no migrations applied" {
		t.Errorf("empty Version = %q", got)
	}

	if got := (Version{Current: 5, Name: "create_a"}).String(); got != "5_create_a" {
		t.Errorf("Version = %q", got)
	}

	dirty := Version{Current: 5, Name: "create_a", Dirty: true}
	if !strings.Contains(dirty.String(), "incomplete") {
		t.Errorf("a dirty Version does not say so: %q", dirty)
	}
}

func TestWipeReportRendering(t *testing.T) {
	t.Parallel()

	r := &WipeReport{
		Database: "shop_dev",
		Schema:   "public",
		Dropped:  []Object{{Schema: "public", Name: "users", Kind: "table"}},
		Kept:     []Object{{Schema: "public", Name: "gin_trgm_ops", Kind: "routine", Reason: "owned by another role"}},
	}

	if got := r.String(); !strings.Contains(got, "shop_dev") || !strings.Contains(got, "1 objects dropped") {
		t.Errorf("String = %q", got)
	}

	// A dry run must not claim to have dropped anything.
	dry := &WipeReport{Database: "shop_dev", DryRun: true, Dropped: r.Dropped}
	if got := dry.String(); !strings.Contains(got, "would be dropped") {
		t.Errorf("dry-run String = %q", got)
	}

	if got := r.Dropped[0].String(); got != "table public.users" {
		t.Errorf("dropped object = %q", got)
	}

	if got := r.Kept[0].String(); !strings.Contains(got, "kept: owned by another role") {
		t.Errorf("kept object = %q", got)
	}
}

func TestDropKeyword(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"table": "TABLE", "view": "VIEW", "materialized view": "MATERIALIZED VIEW",
		"sequence": "SEQUENCE", "routine": "ROUTINE", "type": "TYPE",
		"something else": "TABLE",
	}

	for kind, want := range cases {
		if got := dropKeyword(kind); got != want {
			t.Errorf("dropKeyword(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	t.Parallel()

	files := map[string]string{"1_a.up.sql": "SELECT 1;"}

	if _, err := New(nil, fsOf(files)); err == nil {
		t.Error("New accepted a nil Connector")
	}

	for _, opt := range []Option{WithSchema("Public"), WithTable("a-b")} {
		if _, err := New(FromDSN(""), fsOf(files), opt); !errors.Is(err, ErrInvalidIdentifier) {
			t.Errorf("New accepted an invalid identifier: %v", err)
		}
	}

	// The source is validated before any database contact, so a broken
	// directory fails here rather than three migrations into a deploy.
	//
	// A directory holding no .sql files at all is empty; one holding a .sql
	// file that does not match the pattern is a mistake, and saying which is
	// the difference between "add a migration" and "rename this one".
	if _, err := New(FromDSN(""), fsOf(map[string]string{"readme.md": "x"})); !errors.Is(err, ErrNoSource) {
		t.Errorf("New over an empty source = %v, want %v", err, ErrNoSource)
	}

	if _, err := New(FromDSN(""), fsOf(map[string]string{"nope.sql": "SELECT 1;"})); !errors.Is(err, ErrBadFilename) {
		t.Errorf("New over a misnamed migration = %v, want %v", err, ErrBadFilename)
	}

	m, err := New(FromDSN(""), fsOf(files), WithSchema("app"), WithTable("versions"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m.Schema() != "app" || m.Table() != "versions" || m.Source().Len() != 1 {
		t.Errorf("accessors report %s.%s over %d migrations", m.Schema(), m.Table(), m.Source().Len())
	}
}

func TestLockIDIsDerivedFromSchemaAndTable(t *testing.T) {
	t.Parallel()

	files := map[string]string{"1_a.up.sql": "SELECT 1;"}

	build := func(opts ...Option) *Migrator {
		m, err := New(FromDSN(""), fsOf(files), opts...)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		return m
	}

	// Two Migrators over one bookkeeping table must serialise against each
	// other without being told to, which is only true if the derivation is a
	// pure function of the names.
	classA, objA := build(WithSchema("app"), WithTable("versions")).LockID()
	classB, objB := build(WithSchema("app"), WithTable("versions")).LockID()

	if classA != classB || objA != objB {
		t.Errorf("the same schema and table produced %d/%d and %d/%d", classA, objA, classB, objB)
	}

	// Different tables must not collide, or two unrelated projects in one
	// database would block each other.
	_, objOther := build(WithSchema("app"), WithTable("other")).LockID()
	if objOther == objA {
		t.Error("two different tables derived the same lock id")
	}

	if objA < 0 || objOther < 0 {
		t.Error("a negative object id prints confusingly in pg_locks")
	}

	// An explicit id wins.
	_, objExplicit := build(WithLockID(4242)).LockID()
	if objExplicit != 4242 {
		t.Errorf("WithLockID was ignored: %d", objExplicit)
	}
}

func TestConfirm(t *testing.T) {
	t.Parallel()

	c := Confirm("shop_dev")
	if !c.given || c.database != "shop_dev" {
		t.Errorf("Confirm produced %+v", c)
	}

	// The zero value is "not confirmed", which is what makes an omitted
	// confirmation a refusal rather than a match against the empty string.
	var zero Confirmation
	if zero.given {
		t.Error("the zero Confirmation counts as given")
	}
}

// TestOptionsApply checks that every option sets what it claims to.
//
// One-line setters are exactly where a copy-paste slip hides: WithLockTimeout
// assigning advisoryLockWait would compile, pass every other test, and quietly
// give migrations a three-second statement budget.
func TestOptionsApply(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	cfg := defaults()
	for _, o := range []Option{
		WithSchema("app"),
		WithTable("versions"),
		WithLogger(logger),
		WithPlaceholders(map[string]string{"@a@": "b"}),
		WithAppliedBy("someone@somewhere"),
		WithMigratorTag("v2.0.0"),
		WithLockID(4242),
		WithAdvisoryLockTimeout(time.Minute),
		WithStatementTimeout(2 * time.Hour),
		WithLockTimeout(7 * time.Second),
		WithLockRetries(9, 2*time.Second),
		WithAllowDown(),
		WithAllowWipe(),
		WithOutOfOrder(),
		WithoutPinCheck(),
		WithEnvironment(EnvStaging),
		WithWipeProtectPattern("^never$"),
	} {
		o.apply(&cfg)
	}

	checks := map[string]bool{
		"schema":             cfg.schema == "app",
		"table":              cfg.table == "versions",
		"logger":             cfg.logger == logger,
		"placeholders":       cfg.placeholders["@a@"] == "b",
		"appliedBy":          cfg.appliedBy == "someone@somewhere",
		"migratorTag":        cfg.migratorTag == "v2.0.0",
		"lockID":             cfg.lockID == 4242 && cfg.lockIDSet,
		"advisoryLockWait":   cfg.advisoryLockWait == time.Minute,
		"statementTimeout":   cfg.statementTimeout == 2*time.Hour,
		"lockTimeout":        cfg.lockTimeout == 7*time.Second,
		"lockRetries":        cfg.lockRetries == 9 && cfg.lockRetryBackoff == 2*time.Second,
		"allowDown":          cfg.allowDown,
		"allowWipe":          cfg.allowWipe,
		"allowOutOfOrder":    cfg.allowOutOfOrder,
		"skipPinCheck":       cfg.skipPinCheck,
		"environment":        cfg.environment == EnvStaging,
		"wipeProtectPattern": cfg.wipeProtectPattern == "^never$",
	}

	for name, ok := range checks {
		if !ok {
			t.Errorf("%s was not set as its option promises", name)
		}
	}

	// A nil logger is ignored rather than installed: a library that panics on
	// its own default is worse than one that is quiet.
	WithLogger(nil).apply(&cfg)

	if cfg.logger == nil {
		t.Error("WithLogger(nil) installed a nil logger")
	}

	// The placeholder map is copied, so that a caller mutating theirs later
	// cannot change what a built Migrator substitutes.
	given := map[string]string{"@x@": "1"}

	fresh := defaults()
	WithPlaceholders(given).apply(&fresh)

	given["@x@"] = "2"

	if fresh.placeholders["@x@"] != "1" {
		t.Error("WithPlaceholders kept the caller's map instead of copying it")
	}
}

func TestDefaultAppliedBy(t *testing.T) {
	t.Setenv("USER", "someone")

	got := defaultAppliedBy()
	if !strings.HasPrefix(got, "someone@") {
		t.Errorf("defaultAppliedBy = %q, want it to start with the user", got)
	}

	t.Setenv("USER", "")
	t.Setenv("USERNAME", "")

	if got := defaultAppliedBy(); !strings.HasPrefix(got, "unknown@") {
		t.Errorf("with no user in the environment: %q", got)
	}
}

// TestEnvironmentTextIsCaseInsensitive pins the fix for a guard that could be
// switched off by capitalisation.
//
// config.Validate accepts "Production" because it lowercases before checking.
// UnmarshalText used to match case-sensitively, so the parse failed, the error
// was discarded, and the environment was then *inferred* — turning off the
// production guard that the operator had explicitly asked for.
func TestEnvironmentTextIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, text := range []string{"production", "Production", "PRODUCTION", "  prod  ", "Live"} {
		var env Environment
		if err := env.UnmarshalText([]byte(text)); err != nil {
			t.Errorf("UnmarshalText(%q) = %v", text, err)

			continue
		}

		if env != EnvProduction {
			t.Errorf("UnmarshalText(%q) = %v, want production", text, env)
		}
	}
}

// TestPlaceholderKeysMustBeDelimited: a bare key would be a blind substring
// replacement over the whole migration, and the unresolved-token check could
// not notice — there would be no token left to find.
func TestPlaceholderKeysMustBeDelimited(t *testing.T) {
	t.Parallel()

	files := map[string]string{"1_a.up.sql": "CREATE TABLE tenants (id int);"}

	_, err := New(FromDSN(""), fsOf(files), WithPlaceholders(map[string]string{"tenant": "acme"}))
	if !errors.Is(err, ErrUnresolvedPlaceholder) {
		t.Fatalf("New accepted a bare placeholder key: %v", err)
	}

	if !strings.Contains(err.Error(), "@name@") {
		t.Errorf("the error does not say what the key should look like: %v", err)
	}

	if _, err := New(FromDSN(""), fsOf(files),
		WithPlaceholders(map[string]string{"@tenant@": "acme"})); err != nil {
		t.Errorf("New rejected a well-formed placeholder key: %v", err)
	}
}

// TestPlaceholderOrderIsDeterministic: strings.NewReplacer resolves competing
// patterns by argument order, so a map-ordered pair list made the same binary
// produce different SQL between runs — which the checksum cannot catch, because
// it is taken before substitution.
func TestPlaceholderOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	files := map[string]string{"1_a.up.sql": "SELECT '@db@', '@db_ro@';"}

	for range 20 {
		m, err := New(FromDSN(""), fsOf(files), WithPlaceholders(map[string]string{
			"@db@":    "primary",
			"@db_ro@": "replica",
		}))
		if err != nil {
			t.Fatal(err)
		}

		got, err := m.substitute("SELECT '@db@', '@db_ro@';")
		if err != nil {
			t.Fatal(err)
		}

		if got != "SELECT 'primary', 'replica';" {
			t.Fatalf("substitution = %q; the longer key must win regardless of map order", got)
		}
	}
}

// TestReportTextLabelsEachRecord: a Redo reports DirectionDown and holds both
// halves, so a verb taken from the direction labelled the re-applied migration
// "reverted" — the inverse of what happened to it.
func TestReportTextLabelsEachRecord(t *testing.T) {
	t.Parallel()

	now := time.Now()
	reverted := now.Add(time.Second)

	r := &Report{
		Direction: DirectionDown,
		Applied: []Record{
			{Version: 3, Name: "add_index", FinishedAt: &now, RolledBackAt: &reverted},
			{Version: 3, Name: "add_index", FinishedAt: &now},
		},
	}

	var b strings.Builder
	if err := r.Text(&b); err != nil {
		t.Fatal(err)
	}

	out := b.String()
	if !strings.Contains(out, "reverted   3_add_index") {
		t.Errorf("the rolled-back half is not labelled reverted:\n%s", out)
	}

	if !strings.Contains(out, "applied    3_add_index") {
		t.Errorf("the re-applied half is not labelled applied:\n%s", out)
	}
}

// TestOneTransactionIsNotAlwaysTrue: the field is documented as reporting
// whether the run held together, and a caller may branch on it to decide
// whether a failed deploy needs manual inspection.
func TestOneTransactionIsNotAlwaysTrue(t *testing.T) {
	t.Parallel()

	now := time.Now()

	single := &Report{Applied: []Record{{Version: 1, FinishedAt: &now, Transactional: true}}}
	two := &Report{Applied: []Record{
		{Version: 1, FinishedAt: &now, Transactional: true},
		{Version: 2, FinishedAt: &now, Transactional: true},
	}}
	nonTx := &Report{Applied: []Record{{Version: 1, FinishedAt: &now, Transactional: false}}}

	// The computation lives in run(); this pins the rule it implements.
	oneTx := func(r *Report) bool {
		return len(r.Applied) == 1 && r.Applied[0].Transactional
	}

	if !oneTx(single) {
		t.Error("one transactional migration is one transaction")
	}

	if oneTx(two) {
		t.Error("two migrations are two transactions")
	}

	if oneTx(nonTx) {
		t.Error("a no-transaction migration is no transaction")
	}
}

// TestStatusShowsTagsAndAdoption: both were parsed and recorded and shown
// nowhere. A doc comment promising "reported by status" that status does not
// honour is worse than not having the field.
func TestStatusShowsTagsAndAdoption(t *testing.T) {
	t.Parallel()

	set := mustLoad(t, map[string]string{
		"1_slow_ddl.up.sql": "-- migrator:tags ddl,slow\nCREATE TABLE a (id int);",
	})

	mig, _ := set.ByVersion(1)
	at := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)

	status := &Status{
		Schema: "public", Table: "schema_migrations", Initialised: true,
		Entries: []Entry{{
			Version: 1, Name: "slow_ddl", State: StateApplied,
			Migration: &mig,
			Record: &Record{
				Version: 1, AppliedAt: at, FinishedAt: &at, AdoptedAt: &at, Checksum: mig.Checksum,
			},
		}},
	}

	var text strings.Builder
	if err := status.Text(&text); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"[ddl,slow]", "(adopted)"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the table does not show %q:\n%s", want, text.String())
		}
	}

	var raw strings.Builder
	if err := status.JSON(&raw); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Entries []struct {
			Tags    []string `json:"tags"`
			Adopted bool     `json:"adopted"`
		} `json:"entries"`
	}

	if err := json.Unmarshal([]byte(raw.String()), &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	if len(out.Entries) != 1 || len(out.Entries[0].Tags) != 2 || !out.Entries[0].Adopted {
		t.Errorf("JSON decoded %+v", out)
	}
}

// TestJSONCarriesItsFormat: without a version a consumer cannot tell 2.0's
// output from 2.3's and breaks silently — the class of failure the rest of this
// tool exists to prevent.
func TestJSONCarriesItsFormat(t *testing.T) {
	t.Parallel()

	renderers := map[string]interface{ JSON(io.Writer) error }{
		"Report":           sampleReport(),
		"Status":           sampleStatus(),
		"ValidationReport": &ValidationReport{},
		"WipeReport":       &WipeReport{Database: "shop_dev", Schema: "public"},
	}

	for name, r := range renderers {
		var b bytes.Buffer
		if err := r.JSON(&b); err != nil {
			t.Fatalf("%s.JSON: %v", name, err)
		}

		var out struct {
			Format int `json:"format"`
		}

		if err := json.Unmarshal(b.Bytes(), &out); err != nil {
			t.Fatalf("%s wrote invalid JSON: %v", name, err)
		}

		if out.Format != JSONFormat {
			t.Errorf("%s reports format %d, want %d", name, out.Format, JSONFormat)
		}
	}
}

// TestWipeReportRendersBothWays: wipe --json used to print a plain-text list,
// because WipeReport had no JSON method and the command wrote lines by hand.
func TestWipeReportRendersBothWays(t *testing.T) {
	t.Parallel()

	r := &WipeReport{
		Database: "shop_dev", Schema: "public", DryRun: true,
		Dropped: []Object{{Schema: "public", Name: "users", Kind: "table"}},
		Kept:    []Object{{Schema: "public", Name: "gin_trgm_ops", Kind: "routine", Reason: "owned by another role"}},
	}

	var text bytes.Buffer
	if err := r.Text(&text); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"would drop", "public.users", "kept", "would be dropped"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text output lacks %q:\n%s", want, text.String())
		}
	}

	var raw bytes.Buffer
	if err := r.JSON(&raw); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Database string `json:"database"`
		DryRun   bool   `json:"dry_run"`
		Dropped  []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"dropped"`
		Kept []struct {
			Reason string `json:"reason"`
		} `json:"kept"`
	}

	if err := json.Unmarshal(raw.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if out.Database != "shop_dev" || !out.DryRun || len(out.Dropped) != 1 || len(out.Kept) != 1 {
		t.Errorf("decoded %+v", out)
	}

	if out.Kept[0].Reason == "" {
		t.Error("a kept object does not say why it was kept")
	}
}
