package migrator

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"time"
)

// advisoryClassID namespaces every advisory lock this tool takes.
//
// The two-argument form of pg_try_advisory_lock is used rather than the
// one-argument form so that the class is a fixed, recognisable constant. That
// buys two things the single-int64 form cannot: attribution, because
// `SELECT pid, objid FROM pg_locks WHERE locktype = 'advisory' AND classid = ...`
// then names the process holding it; and the guarantee that a lock taken by
// some other tool with an unlucky key cannot collide with ours.
const advisoryClassID int32 = 0x6D677274 & 0x7FFFFFFF // "mgrt"

// lockID reports the advisory lock identifier this Migrator uses, so that an
// operator can find the holder in pg_locks.
//
// The derivation is part of the API contract and is documented as such:
// changing how objid is computed in a minor release would let two versions of
// this tool migrate one database at the same time, each holding a lock the
// other cannot see.
func (m *Migrator) lockID() (classID, objID int32) {
	if m.cfg.lockIDSet {
		return advisoryClassID, m.cfg.lockID
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(m.cfg.schema + "." + m.cfg.table))

	// Masked to stay positive: PostgreSQL takes a signed int4 and a negative
	// key is legal but prints confusingly in pg_locks.
	return advisoryClassID, int32(h.Sum32() & 0x7FFFFFFF)
}

// LockID reports the advisory lock this Migrator serialises on.
//
// It is exported so that an operator holding a stuck deployment can find the
// session responsible:
//
//	SELECT pid, query FROM pg_locks l JOIN pg_stat_activity a USING (pid)
//	 WHERE l.locktype = 'advisory' AND l.classid = $1 AND l.objid = $2;
//
// Advisory locks are cluster-wide, not per-database, and the identifier is
// derived from the schema and table names only. Two databases on one server
// that both keep their bookkeeping in public.schema_migrations therefore
// serialise their migrations against each other. That is safe — it costs
// waiting, never correctness — and it is what makes two binaries pointed at one
// database serialise without being told to. Where the contention is unwanted,
// give each database its own [WithLockID].
func (m *Migrator) LockID() (classID, objID int32) { return m.lockID() }

// lock takes the advisory lock that serialises concurrent runs and reports a
// function that releases it.
//
// pg_try_advisory_lock in a loop rather than the blocking pg_advisory_lock:
// lock_timeout does not apply to advisory locks, so the blocking form would
// ignore the deadline entirely and sit there with nothing to report.
//
// Session-level rather than pg_advisory_xact_lock: the lock has to outlive many
// transactions, one per migration, and the transaction-scoped form would let go
// at the first COMMIT.
func (m *Migrator) lock(ctx context.Context, s Session) (func(), error) {
	classID, objID := m.lockID()

	wait := m.cfg.advisoryLockWait
	if wait <= 0 {
		wait = 30 * time.Second
	}

	deadline := time.Now().Add(wait)

	for {
		var acquired bool
		if err := s.QueryRow(ctx,
			`SELECT pg_try_advisory_lock($1, $2)`, classID, objID).Scan(&acquired); err != nil {
			return nil, fmt.Errorf("migrator: acquire migration lock: %w", redact(err))
		}

		if acquired {
			release := m.releaser(s, classID, objID)

			if err := m.checkPinned(ctx, s, classID, objID); err != nil {
				release()

				return nil, err
			}

			return release, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: waited %s (advisory lock %d/%d)",
				ErrLockTimeout, wait, classID, objID)
		}

		// Jitter, so that N replicas started by one deployment do not settle
		// into lockstep and retry in unison forever.
		//nolint:gosec // jitter for a retry schedule, not a secret
		pause := 250*time.Millisecond + rand.N(125*time.Millisecond) - 62*time.Millisecond

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}
	}
}

// releaser builds the release function for a held lock.
func (m *Migrator) releaser(s Session, classID, objID int32) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			// A fresh context deliberately detached from the caller's: by the
			// time a run unwinds, the caller's context is often already
			// cancelled, and a session-level advisory lock left held blocks
			// every later run until the connection closes.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
			defer cancel()

			if _, err := s.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, classID, objID); err != nil {
				// A failure here means the connection is already gone, which
				// means the backend is gone, which means PostgreSQL has
				// released the lock for us. Worth a line, not an error.
				m.cfg.logger.Debug("migrator: could not release the advisory lock explicitly",
					"error", redact(err))
			}
		})
	}
}

// checkPinned verifies that the session really is one backend.
//
// pgbouncer in transaction mode breaks session-level advisory locks silently:
// the lock is taken on one backend and the next statement runs on another. A
// silently broken guard against concurrent migrations is the worst outcome
// available here, so it is detected rather than hoped about. The cost is one
// query per run.
func (m *Migrator) checkPinned(ctx context.Context, s Session, classID, objID int32) error {
	if m.cfg.skipPinCheck {
		return nil
	}

	var held int
	if err := s.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND classid = $1 AND objid = $2
		    AND pid = pg_backend_pid()`, classID, objID).Scan(&held); err != nil {
		return fmt.Errorf("migrator: verify the migration lock: %w", redact(err))
	}

	if held == 0 {
		return fmt.Errorf("%w: point the migrator at PostgreSQL directly, "+
			"not at a pooler in transaction mode", ErrSessionNotPinned)
	}

	return nil
}
