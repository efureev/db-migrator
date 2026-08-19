package migrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"time"
)

// A Migrator applies a set of migrations to one PostgreSQL database.
//
// It is immutable once built and safe to use from several goroutines: runs
// serialise against each other through the advisory lock on the server, which
// is also what serialises them against runs in other processes.
type Migrator struct {
	conn Connector
	set  *Set
	cfg  config

	// replacer is the placeholder substitution flattened into the pairs
	// strings.NewReplacer wants; replacerOnce is the Replacer built from them,
	// built here rather than per call because substitute runs once per
	// migration and once more per Plan step.
	replacer     []string
	replacerOnce *strings.Replacer
}

// New builds a Migrator over the migrations in fsys, reachable through c.
//
// The source is read and validated here, before any database contact: a
// malformed file name, a duplicate version, an unknown directive or an empty up
// file fails now rather than three migrations into a deploy. The set is a
// snapshot — a Migrator built over os.DirFS does not notice later edits.
//
// fsys is the directory holding the files, not a directory above it: pass
// os.DirFS("./migrations") or fs.Sub(embedded, "migrations").
func New(c Connector, fsys fs.FS, opts ...Option) (*Migrator, error) {
	if c == nil {
		return nil, fmt.Errorf("migrator: %w", errNilConnector)
	}

	cfg := defaults()
	for _, o := range opts {
		o.apply(&cfg)
	}

	if err := errors.Join(
		validateIdentifier("schema", cfg.schema),
		validateIdentifier("table", cfg.table),
	); err != nil {
		return nil, err
	}

	if cfg.appliedBy == "" {
		cfg.appliedBy = defaultAppliedBy()
	}

	set, err := Load(fsys)
	if err != nil {
		return nil, err
	}

	m := &Migrator{conn: c, set: set, cfg: cfg}

	// The schema is always available as a placeholder, so that a migration
	// written for a configurable schema needs no extra wiring.
	pairs := []string{"@schema@", cfg.schema}

	// Sorted, not in map order. strings.NewReplacer resolves competing patterns
	// by argument order, so with "@db@" and "@db_ro@" the winner would
	// otherwise depend on Go's randomised map iteration — the same binary over
	// the same files producing different SQL between runs, which the checksum
	// cannot catch because it is taken before substitution.
	keys := make([]string, 0, len(cfg.placeholders))

	for k := range cfg.placeholders {
		if !placeholderPattern.MatchString(k) {
			return nil, fmt.Errorf("%w: placeholder key %q must be written as @name@",
				ErrUnresolvedPlaceholder, k)
		}

		keys = append(keys, k)
	}

	// Longest first, so that "@db_ro@" is matched before "@db@" would consume
	// its prefix.
	slices.SortFunc(keys, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}

		return strings.Compare(a, b)
	})

	for _, k := range keys {
		pairs = append(pairs, k, cfg.placeholders[k])
	}

	m.replacer = pairs
	m.replacerOnce = strings.NewReplacer(pairs...)

	return m, nil
}

// errNilConnector reports New called without a connector.
var errNilConnector = errors.New("New was given a nil Connector")

// Source reports the migrations this Migrator was built over.
func (m *Migrator) Source() *Set { return m.set }

// Schema and Table report where the bookkeeping lives.
func (m *Migrator) Schema() string { return m.cfg.schema }

// Table reports the bookkeeping table name.
func (m *Migrator) Table() string { return m.cfg.table }

// Up applies every pending migration.
//
// An up-to-date database is not an error: the report is empty and the error is
// nil. Drift is an error, reported before anything runs.
func (m *Migrator) Up(ctx context.Context) (*Report, error) {
	return m.UpTo(ctx, All())
}

// UpTo applies pending migrations as far as t allows.
func (m *Migrator) UpTo(ctx context.Context, t Target) (*Report, error) {
	return m.run(ctx, DirectionUp, t)
}

// Down rolls back as far as t says.
//
// Unlike [Migrator.Up] it takes no default: there is a sensible answer to "how
// far forward" and none to "how far back". It requires [WithAllowDown] and is
// refused outright under [EnvProduction].
func (m *Migrator) Down(ctx context.Context, t Target) (*Report, error) {
	if err := m.guardDown(); err != nil {
		return nil, err
	}

	return m.run(ctx, DirectionDown, t)
}

// Redo rolls back the last n migrations and applies them again.
//
// Both halves run under one advisory lock, so nothing can slip between them.
// When every migration involved is transactional they also share one
// transaction, and a failure on the way up leaves the database exactly as Redo
// found it; when any of them carries no-transaction that is not possible, and
// [Report.OneTransaction] says so.
func (m *Migrator) Redo(ctx context.Context, n int) (*Report, error) {
	if err := m.guardDown(); err != nil {
		return nil, err
	}

	if n <= 0 {
		n = 1
	}

	return m.redo(ctx, n)
}

// guardDown enforces the two gates on rolling back.
func (m *Migrator) guardDown() error {
	// Production first: it is the refusal no option overrides, and reporting it
	// ahead of the missing flag stops anyone from thinking the flag would help.
	if m.cfg.environment == EnvProduction {
		return fmt.Errorf("%w: rolling back is not available when the environment is production",
			ErrProductionGuard)
	}

	if !m.cfg.allowDown {
		return ErrDownNotAllowed
	}

	return nil
}

// run is the body of Up, UpTo and Down.
func (m *Migrator) run(ctx context.Context, d Direction, t Target) (*Report, error) {
	report := &Report{Direction: d, Target: t, StartedAt: time.Now()}

	err := m.withLock(ctx, func(s Session, applied map[int64]Record) error {
		steps, err := m.planFor(d, t, applied)
		if err != nil {
			return err
		}

		// Under the lock and against the statements about to be sent, so the
		// plan cannot change between the look and the leap.
		if err := m.guardLockLevel(ctx, s, steps, d); err != nil {
			return err
		}

		// After the guards and after planning, so that a run which turned out
		// to have nothing to do — most invocations in a pipeline — opens no
		// second connection at all.
		p := m.progressFor(ctx, s)
		defer p.close(ctx)

		for _, mig := range steps {
			rec, err := m.applyOne(ctx, s, p, mig, d)
			if err != nil {
				return err
			}

			m.cfg.logger.Info("migrator: migration applied",
				"version", mig.Version, "name", mig.Name, "direction", d.String(),
				"took", rec.ExecutionTime)

			report.Applied = append(report.Applied, rec)
		}

		return nil
	})

	report.Duration = time.Since(report.StartedAt)

	if err != nil {
		return report, err
	}

	// True only when the run really was one transaction: a single migration
	// that ran inside one. Two migrations are two transactions, and a
	// no-transaction migration is none. Reporting otherwise would tell a caller
	// deciding whether a failed deploy needs inspecting that the run was atomic
	// when it was not.
	report.OneTransaction = len(report.Applied) == 1 &&
		!slices.ContainsFunc(report.Applied, func(rec Record) bool { return !rec.Transactional })

	return report, nil
}

// redo runs a rollback and a re-apply under one lock.
func (m *Migrator) redo(ctx context.Context, n int) (*Report, error) {
	report := &Report{Direction: DirectionDown, Target: Steps(n), StartedAt: time.Now()}

	err := m.withLock(ctx, func(s Session, applied map[int64]Record) error {
		down, err := m.planFor(DirectionDown, Steps(n), applied)
		if err != nil {
			return err
		}

		if len(down) == 0 {
			return nil
		}

		// Both halves are checked before either runs: refusing on the way back
		// up, after the rollback has already happened, would leave the database
		// somewhere nobody asked for.
		if err := m.guardLockLevel(ctx, s, down, DirectionDown); err != nil {
			return err
		}

		if err := m.guardLockLevel(ctx, s, down, DirectionUp); err != nil {
			return err
		}

		p := m.progressFor(ctx, s)
		defer p.close(ctx)

		for _, mig := range down {
			rec, err := m.applyOne(ctx, s, p, mig, DirectionDown)
			if err != nil {
				return err
			}

			report.Applied = append(report.Applied, rec)
		}

		// Back up in ascending order, over exactly the versions just undone.
		up := slices.Clone(down)
		slices.Reverse(up)

		for _, mig := range up {
			rec, err := m.applyOne(ctx, s, p, mig, DirectionUp)
			if err != nil {
				return err
			}

			report.Applied = append(report.Applied, rec)
		}

		report.OneTransaction = !slices.ContainsFunc(down, func(mig Migration) bool {
			return noTransaction(mig, DirectionDown) || noTransaction(mig, DirectionUp)
		})

		return nil
	})

	report.Duration = time.Since(report.StartedAt)

	return report, err
}

// applyOne dispatches to the transactional or the no-transaction path.
//
// The watcher is started here rather than inside either path, because here is
// the one place both of them pass through: the transactional path sends the
// whole body in a single Exec, the other sends it a statement at a time, and a
// migration that takes an hour is worth reporting on whichever it is.
func (m *Migrator) applyOne(
	ctx context.Context, s Session, p *progressWatcher, mig Migration, d Direction,
) (Record, error) {
	stop := p.watch(ctx, mig, d)
	defer stop()

	if noTransaction(mig, d) {
		return m.applyNoTransaction(ctx, s, mig, d)
	}

	return m.applyTransactional(ctx, s, mig, d)
}

// planFor selects the migrations a direction and target imply.
func (m *Migrator) planFor(d Direction, t Target, applied map[int64]Record) ([]Migration, error) {
	if d == DirectionDown {
		return planDown(m.set, t, applied)
	}

	return planUp(m.set, t, applied, m.cfg.allowOutOfOrder)
}

// withLock acquires a session, takes the advisory lock, bootstraps the
// bookkeeping table, checks for drift and hands the caller what it read.
//
// Bootstrap happens under the lock on purpose: concurrent CREATE TABLE IF NOT
// EXISTS in PostgreSQL can fail with a duplicate key on a system catalogue, and
// that race only ever shows up the day two replicas start together.
func (m *Migrator) withLock(ctx context.Context, fn func(Session, map[int64]Record) error) error {
	s, err := m.conn.Acquire(ctx)
	if err != nil {
		return err
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		s.Release(releaseCtx)
	}()

	release, err := m.lock(ctx, s)
	if err != nil {
		return err
	}
	defer release()

	if err := m.bootstrap(ctx, s); err != nil {
		return err
	}

	applied, err := m.recorded(ctx, s)
	if err != nil {
		return err
	}

	if err := m.checkDrift(applied); err != nil {
		return err
	}

	return fn(s, applied)
}

// Plan reports what a run would do, without doing it.
//
// It performs every check a run performs — drift, pairing, ordering,
// directives — and renders the SQL each step would send, after substitution.
// It takes no lock and writes nothing.
func (m *Migrator) Plan(ctx context.Context, d Direction, t Target) (*Plan, error) {
	// One session for the whole thing: FromDSN opens a connection per Acquire,
	// and a plan that opened two of them to answer one question would be
	// noticeable on a server that counts connections.
	s, err := m.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		s.Release(releaseCtx)
	}()

	applied, err := m.readStateOn(ctx, s)
	if err != nil {
		return nil, err
	}

	if err := m.checkDrift(applied); err != nil {
		return nil, err
	}

	steps, err := m.planFor(d, t, applied)
	if err != nil {
		return nil, err
	}

	built, err := m.stepsFor(steps, d)
	if err != nil {
		return nil, err
	}

	out := &Plan{Direction: d, Target: t, Steps: built}

	m.predict(ctx, s, out.Steps)

	return out, nil
}

// stepsFor renders what each migration would send, in the form it would be
// sent.
func (m *Migrator) stepsFor(migs []Migration, d Direction) ([]Step, error) {
	out := make([]Step, 0, len(migs))

	for _, mig := range migs {
		body, _, _ := halfOf(mig, d)

		sql, err := m.substitute(body)
		if err != nil {
			return nil, err
		}

		step := Step{Migration: mig, Transactional: !noTransaction(mig, d)}

		if step.Transactional {
			// A transactional migration goes to the server whole, so that is
			// what the plan shows.
			step.SQL = []string{sql}
		} else {
			stmts, err := splitForPlan(sql)
			if err != nil {
				return nil, err
			}

			step.SQL = stmts
		}

		out = append(out, step)
	}

	return out, nil
}

// Status reports the source and the database side by side.
//
// It writes nothing: no bootstrap, no lock. A read-only role and a read replica
// must both be able to answer "what is applied here", and a status command that
// creates a table is one nobody dares run against production.
func (m *Migrator) Status(ctx context.Context) (*Status, error) {
	applied, initialised, err := m.readState(ctx)
	if err != nil {
		return nil, err
	}

	status := &Status{Schema: m.cfg.schema, Table: m.cfg.table, Initialised: initialised}

	seen := map[int64]bool{}

	for mig := range m.set.All() {
		seen[mig.Version] = true
		status.Entries = append(status.Entries, entryFor(mig, applied[mig.Version], hasRec(applied, mig.Version)))
	}

	// Rows with no file left: recorded here, gone from the source.
	for version, rec := range applied {
		if seen[version] {
			continue
		}

		state := StateMissing
		if rec.RolledBackAt != nil {
			state = StateRolledBack
		}

		status.Entries = append(status.Entries, Entry{
			Version: version, Name: rec.Name, State: state, Record: ptrOf(rec),
		})
	}

	slices.SortFunc(status.Entries, func(a, b Entry) int {
		switch {
		case a.Version < b.Version:
			return -1
		case a.Version > b.Version:
			return 1
		default:
			return 0
		}
	})

	return status, nil
}

// Version reports where the database stands.
func (m *Migrator) Version(ctx context.Context) (Version, error) {
	status, err := m.Status(ctx)
	if err != nil {
		return Version{}, err
	}

	v := Version{Current: status.Current(), Pending: len(status.Pending())}

	for _, e := range status.Entries {
		if e.State == StateIncomplete {
			v.Dirty = true
		}

		if e.Version == v.Current && e.Record != nil {
			v.Name = e.Name
			v.AppliedAt = e.Record.AppliedAt
		}
	}

	return v, nil
}

// readState acquires a session and reads the bookkeeping table without taking
// the lock or creating anything.
func (m *Migrator) readState(ctx context.Context) (applied map[int64]Record, initialised bool, err error) {
	s, err := m.conn.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		s.Release(releaseCtx)
	}()

	initialised, err = m.initialised(ctx, s)
	if err != nil {
		return nil, false, err
	}

	if !initialised {
		// Nothing has been applied and nothing is created to find that out.
		return map[int64]Record{}, false, nil
	}

	applied, err = m.recorded(ctx, s)
	if err != nil {
		return nil, false, err
	}

	return applied, true, nil
}

// readStateOn reads the bookkeeping table over a session the caller already
// holds, for the callers that need the same session afterwards.
func (m *Migrator) readStateOn(ctx context.Context, s Session) (map[int64]Record, error) {
	initialised, err := m.initialised(ctx, s)
	if err != nil {
		return nil, err
	}

	if !initialised {
		return map[int64]Record{}, nil
	}

	return m.recorded(ctx, s)
}

// entryFor pairs one migration with its row, if it has one.
func entryFor(mig Migration, rec Record, recorded bool) Entry {
	e := Entry{Version: mig.Version, Name: mig.Name, Migration: ptrOf(mig)}

	if !recorded {
		e.State = StatePending

		return e
	}

	e.Record = ptrOf(rec)

	switch {
	case rec.Incomplete():
		e.State = StateIncomplete
	case rec.RolledBackAt != nil:
		e.State = StateRolledBack
	case rec.Checksum != mig.Checksum:
		e.State = StateModified
	default:
		e.State = StateApplied
	}

	return e
}

func hasRec(applied map[int64]Record, v int64) bool {
	_, ok := applied[v]

	return ok
}

func ptrOf[T any](v T) *T { return &v }

// defaultAppliedBy reports who is running this, for the bookkeeping row.
func defaultAppliedBy() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}

	if user == "" {
		user = "unknown"
	}

	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}

	return user + "@" + host
}

// randN is a seam over the global rand, so that backoff jitter has one place to
// come from.
func randN(n int64) int64 {
	if n <= 0 {
		return 0
	}

	// Jitter for a retry schedule, not a secret. A predictable pause costs
	// nothing here: the point is only that concurrent runners stop retrying in
	// unison, and an attacker who can predict it gains a 250ms head start on
	// a lock they still cannot take.
	return rand.N(n) //nolint:gosec // jitter, not a secret
}
