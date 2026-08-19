package migrator

import (
	"log/slog"
	"time"
)

// An Option configures a [Migrator].
//
// The interface is sealed — its only method is unexported — so that no caller
// outside this package can construct one. For a package under SemVer that
// matters: it keeps the configuration struct entirely internal, and therefore
// free to change, which a `type Option func(*Config)` with an exported Config
// would not.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

// config is everything a Migrator was built with.
type config struct {
	schema, table string
	logger        *slog.Logger
	placeholders  map[string]string
	appliedBy     string
	migratorTag   string

	lockID             int32
	lockIDSet          bool
	advisoryLockWait   time.Duration
	statementTimeout   time.Duration
	lockTimeout        time.Duration
	lockRetries        int
	lockRetryBackoff   time.Duration
	skipPinCheck       bool
	allowDown          bool
	allowWipe          bool
	allowOutOfOrder    bool
	environment        Environment
	wipeProtectPattern string
}

func defaults() config {
	return config{
		schema:           "public",
		table:            "schema_migrations",
		logger:           slog.New(slog.DiscardHandler),
		advisoryLockWait: 30 * time.Second,
		// Zero, but always sent: what matters is not the value, it is that the
		// run overrides whatever it inherited. A pool configured with
		// statement_timeout=30s otherwise kills exactly the long migrations
		// this tool exists to run.
		statementTimeout: 0,
		// Three seconds, deliberately not zero. This is the one default worth
		// making an opinion: without a lock_timeout, an ALTER TABLE queued
		// behind a long-running SELECT queues every later query behind itself,
		// which is not a slow migration, it is an outage. A short lock_timeout
		// turns that outage into a failed deploy.
		lockTimeout:        3 * time.Second,
		lockRetries:        3,
		lockRetryBackoff:   time.Second,
		wipeProtectPattern: `(?i)prod`,
	}
}

// WithSchema sets the schema holding the bookkeeping table. Default "public".
//
// It also seeds the "@schema@" placeholder, so a migration written for a
// configurable schema needs no further wiring.
func WithSchema(name string) Option {
	return optionFunc(func(c *config) { c.schema = name })
}

// WithTable sets the bookkeeping table name. Default "schema_migrations".
func WithTable(name string) Option {
	return optionFunc(func(c *config) { c.table = name })
}

// WithLogger sets the logger. The default discards everything: a library that
// logs where it was not asked to is a library that corrupts somebody's JSON
// output on stdout.
func WithLogger(l *slog.Logger) Option {
	return optionFunc(func(c *config) {
		if l != nil {
			c.logger = l
		}
	})
}

// WithPlaceholders sets the textual substitutions applied to a migration body
// before it is sent.
//
// Substitution is textual and unconditional — it does not know SQL — so a
// placeholder must stand only where an identifier stands, and an @name@ inside
// a string literal is substituted too. The checksum covers the file before
// substitution, so pointing two deployments at two schemas is not mistaken for
// somebody having edited a released file.
//
// A token of the form @name@ left unresolved after substitution is an error: a
// mistyped @tabel@ must not reach PostgreSQL as a syntax error halfway through
// a DDL script.
func WithPlaceholders(m map[string]string) Option {
	return optionFunc(func(c *config) {
		c.placeholders = make(map[string]string, len(m))
		for k, v := range m {
			c.placeholders[k] = v
		}
	})
}

// WithAppliedBy sets what goes into the applied_by column. The default is
// "<user>@<host>".
func WithAppliedBy(who string) Option {
	return optionFunc(func(c *config) { c.appliedBy = who })
}

// WithMigratorTag sets what goes into the migrator column, normally the version
// of the binary doing the applying.
func WithMigratorTag(tag string) Option {
	return optionFunc(func(c *config) { c.migratorTag = tag })
}

// WithLockID namespaces the advisory lock.
//
// Unset, it is derived from the schema and table names, so that two different
// binaries pointed at one bookkeeping table serialise against each other
// without being told to. Override it only to separate runs that share a table
// and must not.
func WithLockID(id int32) Option {
	return optionFunc(func(c *config) { c.lockID, c.lockIDSet = id, true })
}

// WithAdvisoryLockTimeout bounds how long to wait for another run to finish.
// Default 30s.
func WithAdvisoryLockTimeout(d time.Duration) Option {
	return optionFunc(func(c *config) { c.advisoryLockWait = d })
}

// WithStatementTimeout sets statement_timeout for the duration of the run.
//
// Default 0, meaning no limit: a CREATE INDEX on a large table legitimately
// runs for an hour. The value is always sent, including zero — see [defaults].
func WithStatementTimeout(d time.Duration) Option {
	return optionFunc(func(c *config) { c.statementTimeout = d })
}

// WithLockTimeout sets PostgreSQL's lock_timeout for the duration of the run.
// Default 3s; see [defaults] for why it is not zero.
func WithLockTimeout(d time.Duration) Option {
	return optionFunc(func(c *config) { c.lockTimeout = d })
}

// WithLockRetries retries a statement that failed with lock_not_available
// (SQLSTATE 55P03). Default 3 retries a second apart.
//
// Retrying is offered only on the transactional path, where the failed attempt
// rolled back whole. A no-transaction migration is never retried.
func WithLockRetries(n int, backoff time.Duration) Option {
	return optionFunc(func(c *config) { c.lockRetries, c.lockRetryBackoff = n, backoff })
}

// WithAllowDown permits [Migrator.Down] and [Migrator.Redo]. Without it both
// refuse with [ErrDownNotAllowed].
func WithAllowDown() Option {
	return optionFunc(func(c *config) { c.allowDown = true })
}

// WithAllowWipe permits [Migrator.Wipe]. Without it Wipe refuses with
// [ErrWipeRefused], which is what makes a Migrator built by an application
// physically unable to erase the schema.
func WithAllowWipe() Option {
	return optionFunc(func(c *config) { c.allowWipe = true })
}

// WithOutOfOrder permits applying a pending migration whose version is below
// the current one.
//
// Off by default: two long-lived branches merged in either order must not
// produce two different schemas.
func WithOutOfOrder() Option {
	return optionFunc(func(c *config) { c.allowOutOfOrder = true })
}

// WithoutPinCheck disables the check that the session stays on one backend.
//
// The check costs one query per run and closes a whole class of
// unreproducible incidents; disable it only for a proxy that is known to be
// session-pinned and that the check misreads.
func WithoutPinCheck() Option {
	return optionFunc(func(c *config) { c.skipPinCheck = true })
}

// WithEnvironment declares what kind of database this is, gating the
// destructive operations. See [Environment].
func WithEnvironment(env Environment) Option {
	return optionFunc(func(c *config) { c.environment = env })
}

// An Environment says what kind of database a [Migrator] is pointed at.
type Environment uint8

const (
	// EnvUnknown is the default. Destructive operations are permitted with the
	// options that allow them, and demand a confirmation naming the database.
	EnvUnknown Environment = iota
	// EnvDevelopment permits every operation the options allow.
	EnvDevelopment
	// EnvStaging behaves as EnvUnknown.
	EnvStaging
	// EnvProduction refuses Down, Redo and Wipe outright, whatever the options
	// say. It is the one setting no flag overrides.
	EnvProduction
)

// String reports the environment as it is spelled in configuration.
func (e Environment) String() string {
	switch e {
	case EnvDevelopment:
		return "development"
	case EnvStaging:
		return "staging"
	case EnvProduction:
		return "production"
	case EnvUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// MarshalText implements [encoding.TextMarshaler].
func (e Environment) MarshalText() ([]byte, error) { return []byte(e.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler], accepting the short
// spellings people actually put in MIGRATOR_ENV.
func (e *Environment) UnmarshalText(b []byte) error {
	switch string(b) {
	case "development", "dev", "test", "local":
		*e = EnvDevelopment
	case "staging", "stage":
		*e = EnvStaging
	case "production", "prod", "live":
		*e = EnvProduction
	case "", "unknown":
		*e = EnvUnknown
	default:
		return &SourceError{Detail: string(b), Err: ErrInvalidIdentifier}
	}

	return nil
}

// WithWipeProtectPattern sets the regular expression that refuses a wipe by
// database name. The default is `(?i)prod`.
//
// It is cheap and catches the most common version of the accident. An empty
// pattern disables the check.
func WithWipeProtectPattern(pattern string) Option {
	return optionFunc(func(c *config) { c.wipeProtectPattern = pattern })
}
