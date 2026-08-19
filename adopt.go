package migrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Errors reported by Adopt.
var (
	// ErrAlreadyAdopted reports a journal that already holds rows. Adoption is
	// for a database this tool has not managed before; running it over a
	// managed one would rewrite history that was actually observed.
	ErrAlreadyAdopted = errors.New("migrator: the journal already has rows")
	// ErrDirtyJournal reports a golang-migrate journal whose dirty flag is set.
	//
	// That flag means a migration failed partway and nobody recorded what state
	// it left the schema in. Adopting such a database would freeze an unknown
	// state as the truth, and every later checksum comparison would be against
	// a baseline nobody verified.
	ErrDirtyJournal = errors.New("migrator: the existing journal is marked dirty")
	// ErrNoBaseline reports an adoption with nothing to adopt to.
	ErrNoBaseline = errors.New("migrator: no baseline version given and none could be read")
)

// AdoptOptions says which migrations to record as already applied.
type AdoptOptions struct {
	// Baseline is the highest version to record. Every migration at or below it
	// is marked applied; everything above stays pending.
	Baseline int64
	// FromGolangMigrate reads the baseline out of an existing golang-migrate
	// journal — the (version, dirty) table that shares this tool's default name
	// — instead of taking it from Baseline.
	FromGolangMigrate bool
	// Force allows adoption over a journal that already has rows. It exists for
	// the second attempt after a first one was interrupted, and for nothing
	// else.
	Force bool
	// DryRun reports what would be recorded and records nothing.
	DryRun bool
}

// legacyTable is where a golang-migrate journal is moved when this tool takes
// over. Renamed rather than dropped: a rollback to the old tool has to stay
// possible for at least a day, and the row it needs is the only copy.
const legacySuffix = "_pre_v2"

// Adopt records migrations as applied without running them.
//
// # What it is for
//
// A database that already has a schema — built by hand, by golang-migrate, or
// by an earlier tool — cannot be handed to this one otherwise: Up would try to
// apply migration 1 against tables that already exist. Adoption writes the
// journal that would have existed had this tool built the schema, and takes the
// operator's word that the schema matches the files.
//
// That word is the whole risk, and it is why the rows are marked: [Record.Adopted]
// stays true forever, and status says so. "Applied" and "we were told it was
// applied" are different claims, and only one of them was observed here.
//
// # What it does not do
//
// It runs no migration SQL and touches no schema object other than renaming a
// foreign journal aside. A database whose schema does *not* match the files is
// not made to match by adopting it — it is made to look as though it does,
// which is worse. Check first.
func (m *Migrator) Adopt(ctx context.Context, c Confirmation, o AdoptOptions) (*Report, error) {
	report := &Report{Direction: DirectionUp, Target: All(), StartedAt: time.Now()}

	err := m.adopt(ctx, c, o, report)

	report.Duration = time.Since(report.StartedAt)

	if err != nil {
		return nil, err
	}

	return report, nil
}

func (m *Migrator) adopt(ctx context.Context, c Confirmation, o AdoptOptions, report *Report) error {
	s, err := m.conn.Acquire(ctx)
	if err != nil {
		return err
	}

	defer func() { s.Release(context.WithoutCancel(ctx)) }()

	var database string
	if err := s.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return fmt.Errorf("migrator: read the current database: %w", redact(err))
	}

	// Adoption writes a claim into the database that nobody will re-check
	// later, so outside development it names the database out loud.
	if err := m.confirmAdopt(c, database); err != nil {
		return err
	}

	release, err := m.lock(ctx, s)
	if err != nil {
		return err
	}
	defer release()

	baseline := o.Baseline

	if o.FromGolangMigrate {
		baseline, err = m.readLegacyBaseline(ctx, s)
		if err != nil {
			return err
		}
	}

	if baseline <= 0 {
		return fmt.Errorf("%w: pass --baseline <version> or --from-golang-migrate", ErrNoBaseline)
	}

	if _, ok := m.set.ByVersion(baseline); !ok {
		return fmt.Errorf("%w: baseline %d is not in the source", ErrMissingMigration, baseline)
	}

	// The foreign journal is moved aside before ours is created, because both
	// want the same name.
	if o.FromGolangMigrate && !o.DryRun {
		if err := m.renameLegacy(ctx, s); err != nil {
			return err
		}
	}

	if o.DryRun {
		report.DryRun = true

		return m.planAdoption(baseline, report)
	}

	if err := m.bootstrap(ctx, s); err != nil {
		return err
	}

	existing, err := m.recorded(ctx, s)
	if err != nil {
		return err
	}

	if len(existing) > 0 && !o.Force {
		return fmt.Errorf("%w: %d row(s) in %s — adoption is for a database this tool has not managed",
			ErrAlreadyAdopted, len(existing), m.qualified())
	}

	for mig := range m.set.All() {
		if mig.Version > baseline {
			break
		}

		rec, err := m.recordAdopted(ctx, s, mig)
		if err != nil {
			return err
		}

		report.Applied = append(report.Applied, rec)
	}

	m.cfg.logger.Info("migrator: adopted",
		"database", database, "baseline", baseline, "recorded", len(report.Applied))

	return nil
}

// planAdoption fills the report without writing anything.
func (m *Migrator) planAdoption(baseline int64, report *Report) error {
	now := time.Now()

	for mig := range m.set.All() {
		if mig.Version > baseline {
			break
		}

		report.Applied = append(report.Applied, Record{
			Version: mig.Version, Name: mig.Name, Checksum: mig.Checksum,
			AppliedAt: now, FinishedAt: &now, AdoptedAt: &now,
			AppliedBy: m.cfg.appliedBy, Transactional: true, Migrator: m.cfg.migratorTag,
		})
	}

	return nil
}

// confirmAdopt applies the same naming rule as Wipe.
func (m *Migrator) confirmAdopt(c Confirmation, database string) error {
	if c.given && c.database != database {
		return fmt.Errorf("%w: confirmation names %q, connected to %q",
			ErrNotConfirmed, c.database, database)
	}

	if !c.given && m.cfg.environment != EnvDevelopment {
		return fmt.Errorf("%w: name the database to confirm (it is %q)", ErrNotConfirmed, database)
	}

	return nil
}

// readLegacyBaseline reads the version out of a golang-migrate journal.
func (m *Migrator) readLegacyBaseline(ctx context.Context, s Session) (int64, error) {
	columns, err := m.bookkeepingColumnsPresent(ctx, s)
	if err != nil {
		return 0, err
	}

	if len(columns) == 0 {
		return 0, fmt.Errorf("%w: %s does not exist", ErrNoBaseline, m.qualified())
	}

	if !columns["dirty"] || !columns["version"] {
		return 0, fmt.Errorf("%w: %s is not a golang-migrate journal (it has no version+dirty)",
			ErrNoBaseline, m.qualified())
	}

	var (
		version int64
		dirty   bool
	)

	err = s.QueryRow(ctx,
		"SELECT version, dirty FROM "+m.qualified()+" ORDER BY version DESC LIMIT 1").
		Scan(&version, &dirty)
	if err != nil {
		return 0, fmt.Errorf("%w: read %s: %w", ErrNoBaseline, m.qualified(), redact(err))
	}

	if dirty {
		return 0, fmt.Errorf("%w: version %d is marked dirty, so what it left behind is unknown — "+
			"resolve it with the old tool first", ErrDirtyJournal, version)
	}

	return version, nil
}

// renameLegacy moves a foreign journal aside.
func (m *Migrator) renameLegacy(ctx context.Context, s Session) error {
	target := m.cfg.table + legacySuffix

	_, err := s.Exec(ctx, fmt.Sprintf("ALTER TABLE %s RENAME TO %s",
		m.qualified(), pgx.Identifier{target}.Sanitize()))
	if err != nil {
		return fmt.Errorf("%w: move the old journal aside: %w", ErrBookkeeping, redact(err))
	}

	m.cfg.logger.Info("migrator: moved the old journal aside",
		"from", m.cfg.table, "to", target)

	return nil
}

// recordAdopted writes one row that was never watched running.
func (m *Migrator) recordAdopted(ctx context.Context, s Session, mig Migration) (Record, error) {
	rec, err := scanRecord(s.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s (version, name, checksum, down_checksum, applied_at, finished_at,
		                applied_by, transactional, migrator, adopted_at)
		VALUES ($1, $2, $3, $4, now(), now(), $5, TRUE, $6, now())
		ON CONFLICT (version) DO UPDATE
		   SET name = EXCLUDED.name, checksum = EXCLUDED.checksum,
		       down_checksum = EXCLUDED.down_checksum, adopted_at = now(),
		       finished_at = now(), rolled_back_at = NULL
		RETURNING %s`, m.qualified(), recordColumns()),
		mig.Version, mig.Name, mig.Checksum, nullable(mig.DownChecksum),
		m.cfg.appliedBy, m.cfg.migratorTag))
	if err != nil {
		return Record{}, fmt.Errorf("%w: adopt %s: %w", ErrBookkeeping, mig, redact(err))
	}

	return rec, nil
}
