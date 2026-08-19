package migrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/efureev/db-migrator/v2/internal/naming"
)

// ErrBadName reports a migration name that cannot be turned into a file name.
var ErrBadName = errors.New("migrator: name must contain ASCII letters or digits")

// A VersionFormat says how [Create] numbers a new migration.
type VersionFormat uint8

const (
	// VersionTimestamp numbers migrations 20060102150405, in UTC. It is the
	// default: two branches created on the same day still get different
	// versions, which is what stops a merge from producing a duplicate.
	VersionTimestamp VersionFormat = iota
	// VersionUnix numbers migrations with a Unix timestamp, which is what
	// version 1 of this tool did. Files written this way sort before
	// timestamp-formatted ones both numerically and lexically, so the two can
	// coexist in one directory.
	VersionUnix
	// VersionSequential numbers migrations 1, 2, 3 …, continuing from the
	// highest version already in the directory. Readable, and a merge conflict
	// waiting to happen in a team of more than one.
	VersionSequential
)

// A CreateOption configures [Create].
type CreateOption interface {
	applyCreate(*createConfig)
}

type createOptionFunc func(*createConfig)

func (f createOptionFunc) applyCreate(c *createConfig) { f(c) }

type createConfig struct {
	format   VersionFormat
	withDown bool
	upBody   string
	downBody string
	now      func() time.Time
}

// WithVersionFormat sets how the new migration is numbered.
func WithVersionFormat(f VersionFormat) CreateOption {
	return createOptionFunc(func(c *createConfig) { c.format = f })
}

// WithoutDown writes only the up file, for a project that has decided it does
// not roll back.
func WithoutDown() CreateOption {
	return createOptionFunc(func(c *createConfig) { c.withDown = false })
}

// WithTemplate sets the bodies written into the new files.
func WithTemplate(up, down string) CreateOption {
	return createOptionFunc(func(c *createConfig) { c.upBody, c.downBody = up, down })
}

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) CreateOption {
	return createOptionFunc(func(c *createConfig) { c.now = now })
}

// A Pair is what [Create] wrote.
type Pair struct {
	// Version and Name identify the new migration.
	Version int64
	Name    string
	// UpPath and DownPath are the files written. DownPath is empty under
	// [WithoutDown].
	UpPath   string
	DownPath string
}

// defaultUpTemplate is what a new up file starts as.
//
// The directives are listed here, commented out, so that somebody reaching for
// CREATE INDEX CONCURRENTLY finds the answer in the file they are already
// editing rather than in a README they have not opened.
const defaultUpTemplate = `-- Directives, if this migration needs them. They must stay above the SQL.
--
-- migrator:no-transaction      -- required by CREATE INDEX CONCURRENTLY
--                                 and ALTER TYPE ... ADD VALUE
-- migrator:retry-safe          -- this migration is idempotent and may be
--                                 re-run after an interrupted attempt
-- migrator:statement-timeout 30m
-- migrator:lock-timeout 5s

`

const defaultDownTemplate = `-- Undo the up file. Leave it empty only if this migration cannot be undone —
-- an empty down file rolls back successfully and changes nothing, which is
-- worse than not shipping one at all.

`

// Create writes a new pair of migration files into dir and reports their paths.
//
// It needs no database, and the CLI command that wraps it does not open one:
// creating a migration is a thing people do offline, on a plane, before the
// connection details exist.
//
// Both files are written or neither is, and neither overwrites an existing
// file: the flags are O_CREATE|O_EXCL, not O_TRUNC.
func Create(dir, name string, opts ...CreateOption) (Pair, error) {
	cfg := createConfig{
		format:   VersionTimestamp,
		withDown: true,
		upBody:   defaultUpTemplate,
		downBody: defaultDownTemplate,
		now:      time.Now,
	}

	for _, o := range opts {
		o.applyCreate(&cfg)
	}

	snake := naming.Snake(name)
	if snake == "" {
		return Pair{}, fmt.Errorf("%w: %q", ErrBadName, name)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Pair{}, fmt.Errorf("migrator: create the migrations directory: %w", err)
	}

	version, err := nextVersion(dir, cfg)
	if err != nil {
		return Pair{}, err
	}

	pair := Pair{
		Version: version,
		Name:    snake,
		UpPath:  filepath.Join(dir, naming.Format(version, snake, naming.Up)),
	}

	if err := writeNew(pair.UpPath, cfg.upBody); err != nil {
		return Pair{}, err
	}

	if !cfg.withDown {
		return pair, nil
	}

	pair.DownPath = filepath.Join(dir, naming.Format(version, snake, naming.Down))

	if err := writeNew(pair.DownPath, cfg.downBody); err != nil {
		// Both or neither: a half-created migration is a version claimed by a
		// pair that does not exist, and the next Create would pick the same
		// number.
		_ = os.Remove(pair.UpPath)

		return Pair{}, err
	}

	return pair, nil
}

// nextVersion reports the number the new migration should carry.
func nextVersion(dir string, cfg createConfig) (int64, error) {
	switch cfg.format {
	case VersionUnix:
		return cfg.now().UTC().Unix(), nil

	case VersionSequential:
		highest, err := highestVersion(dir)
		if err != nil {
			return 0, err
		}

		return highest + 1, nil

	default: // VersionTimestamp
		v, err := strconvVersion(cfg.now().UTC().Format("20060102150405"))
		if err != nil {
			return 0, err
		}

		return v, nil
	}
}

// highestVersion reports the largest version already in dir, or 0.
func highestVersion(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("migrator: read the migrations directory: %w", err)
	}

	var highest int64

	for _, e := range entries {
		f, err := naming.Parse(e.Name())
		if err != nil {
			continue
		}

		if f.Version > highest {
			highest = f.Version
		}
	}

	return highest, nil
}

// writeNew writes a file, refusing to overwrite one that is already there.
func writeNew(path, body string) error {
	// The path is built from a version this package generated and a name it
	// normalised, joined onto the directory the operator named. O_EXCL is the
	// part that matters: it refuses to overwrite.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is built, not taken
	if err != nil {
		return fmt.Errorf("migrator: create %s: %w", path, err)
	}

	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(path)

		return fmt.Errorf("migrator: write %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("migrator: close %s: %w", path, err)
	}

	return nil
}
