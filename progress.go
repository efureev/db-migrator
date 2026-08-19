package migrator

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/efureev/db-migrator/v2/internal/pgprogress"
)

const (
	// minProgressInterval is the floor on how often the backend is polled. The
	// reading is a catalogue query on a busy server, and there is nothing worth
	// knowing about a migration that changes ten times a second.
	minProgressInterval = 100 * time.Millisecond
	// progressAcquireTimeout bounds the wait for the second connection. With a
	// pool of one it would otherwise wait for the connection the run itself is
	// holding, which is a wait that ends when the process does.
	progressAcquireTimeout = 3 * time.Second
	// maxProgressFailures is how many consecutive failed polls end the
	// reporting. Neither "give up at the first error" nor "retry forever": a
	// connection that has died would otherwise write a debug line every
	// interval for the rest of an hour-long migration.
	maxProgressFailures = 3
)

// progressWatcher polls a second connection for what the run's backend is doing
// and writes one line every interval.
//
// Concurrency: every field is written on the run's goroutine before a watcher
// starts, and read by at most one watcher goroutine at a time. stop() joins
// that goroutine before the next one is started, and the close/receive pair is
// the happens-before edge that makes the plain fields correct. There is never
// more than one watcher at a time because applyOne is sequential.
type progressWatcher struct {
	m        *Migrator
	interval time.Duration
	session  Session
	reader   *pgprogress.Reader
	saved    map[string]string
	pid      int
	fails    int
	off      bool
}

// progressFor prepares progress reporting for a run that has work to do.
//
// Everything here happens on the run's own goroutine, while the pinned session
// is idle, and that is not an accident of where the call sits. With [FromConn]
// the connector hands back the connection the run is about to use, and asking
// that connection for its pid from a goroutine while a migration is in flight
// would interleave two conversations on one protocol stream — which is not a
// wait, it is a corrupted connection. Reading both pids here, before the first
// statement, is what makes the check safe for every Connector, including one a
// caller wrote.
//
// Nothing here can fail the run. A connector that cannot give a second backend,
// a server that will not answer, a pool with a single connection in it: each of
// them means no progress reporting and one line at debug level.
func (m *Migrator) progressFor(ctx context.Context, s Session) *progressWatcher {
	interval := m.cfg.progressInterval
	if interval <= 0 {
		return nil
	}

	interval = max(interval, minProgressInterval)

	// The default logger discards everything, and slog says so before the work
	// is done rather than after. So a library caller who never asked for a
	// logger does not pay for a second connection, and neither does --quiet.
	if !m.cfg.logger.Enabled(ctx, slog.LevelInfo) {
		return nil
	}

	var runPID int
	if err := s.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&runPID); err != nil {
		m.progressOff("could not read the pid of the run's own backend", "error", redact(err))

		return nil
	}

	c := m.cfg.progressConn
	if c == nil {
		c = m.conn
	}

	acquireCtx, cancel := context.WithTimeout(ctx, progressAcquireTimeout)
	defer cancel()

	mon, err := c.Acquire(acquireCtx)
	if err != nil {
		m.progressOff("no second connection to watch the run with", "error", redact(err))

		return nil
	}

	p := &progressWatcher{m: m, interval: interval, session: mon, pid: runPID}

	if !p.confirmSecondBackend(ctx, runPID) {
		p.close(ctx)

		return nil
	}

	// Deliberately not applySettings: that one sets statement_timeout from the
	// run's own configuration, which defaults to unlimited — the opposite of
	// what a monitoring query wants. Captured and restored because with
	// FromPool this connection goes back to somebody's application afterwards.
	p.saved = m.captureSettings(ctx, mon)

	if _, err := mon.Exec(ctx, `SET statement_timeout = '5000ms'`); err != nil {
		m.progressOff("could not bound the watching connection", "error", redact(err))
		p.close(ctx)

		return nil
	}

	reader, err := pgprogress.New(ctx, mon)
	if err != nil {
		m.progressOff("could not ask which progress views this server has", "error", redact(err))
		p.close(ctx)

		return nil
	}

	p.reader = reader

	return p
}

// confirmSecondBackend reports whether the watching session is a different
// backend from the one that will run the migration.
func (p *progressWatcher) confirmSecondBackend(ctx context.Context, runPID int) bool {
	var monPID int
	if err := p.session.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&monPID); err != nil {
		p.m.progressOff("could not read the pid of the watching connection", "error", redact(err))

		return false
	}

	if monPID == runPID {
		// The connector handed back the connection the run is about to use:
		// FromConn, or somebody's wrapper around a single connection. It is a
		// documented limitation rather than a failure.
		p.m.progressOff("the connector cannot give a second connection", "pid", runPID)

		return false
	}

	return true
}

// progressOff records why a run will say nothing while it works.
//
// One shape for every such line, because the question being answered is always
// the same one: the migration took an hour and printed nothing, why.
func (m *Migrator) progressOff(reason string, args ...any) {
	m.cfg.logger.Debug("migrator: progress reporting is off",
		append([]any{"reason", reason}, args...)...)
}

// watch starts reporting on one migration and reports the function that stops
// it.
//
// stop blocks until the watcher goroutine has returned. That is what
// guarantees both that no line can be written about a migration that has
// already finished, and that no poll is still in flight when the watching
// session is released. Neither shows up in a passing test; both show up under
// -race, once, on somebody else's machine.
func (p *progressWatcher) watch(ctx context.Context, mig Migration, d Direction) (stop func()) {
	if p == nil || p.off {
		return func() {}
	}

	var (
		done     = make(chan struct{})
		finished = make(chan struct{})
		started  = time.Now()
	)

	go func() {
		defer close(finished)

		// The first line comes after one interval and never at once: a
		// migration that takes 200ms must produce no commentary at all.
		t := time.NewTicker(p.interval)
		defer t.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !p.report(ctx, mig, d, time.Since(started)) {
					return
				}
			}
		}
	}()

	return func() {
		close(done)
		<-finished
	}
}

// report writes one line and reports whether polling should continue.
func (p *progressWatcher) report(
	ctx context.Context, mig Migration, d Direction, elapsed time.Duration,
) bool {
	snap, ok, err := p.reader.Read(ctx, p.session, p.pid)

	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The run is being torn down. That is an ending, not a failure, and it
		// is not worth a line.
		return false

	case err != nil:
		p.fails++

		p.m.cfg.logger.Debug("migrator: could not read the run's progress", "error", redact(err))

		if p.fails >= maxProgressFailures {
			p.off = true

			p.m.progressOff("the watching connection keeps failing")

			return false
		}

		return true
	}

	p.fails = 0

	// No row: the backend is gone. Saying so is the caller's business, not a
	// progress report's, and the run is about to end anyway.
	if !ok {
		return true
	}

	if snap.Restricted() {
		// The row is there and every column of it is blank, which is what a
		// role without pg_read_all_stats sees of another role's session. That
		// will not start being true later.
		p.off = true

		p.m.progressOff("the watching connection may not see the run's backend; " +
			"it needs the same role, or pg_read_all_stats")

		return false
	}

	// Between statements there is nothing running to report on. "idle in
	// transaction" is a different matter and is reported: a migration parked
	// inside an open transaction is holding every lock it has taken.
	if snap.State == "idle" {
		return true
	}

	p.m.cfg.logger.Info(progressMessage(snap), progressAttrs(mig, d, p.pid, elapsed, snap)...)

	return true
}

// close restores the watching connection and hands it back.
func (p *progressWatcher) close(ctx context.Context) {
	if p == nil || p.session == nil {
		return
	}

	if p.saved != nil {
		p.m.restoreSettings(p.session, p.saved)
	}

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	p.session.Release(releaseCtx)

	p.session = nil
}

// progressMessage names the event.
//
// Two messages rather than one with a field to tell them apart: a consumer of
// the JSON form filters on the message, and "the index is 30% built" and "it is
// alive and has said nothing for fourteen minutes" are different events to be
// told about.
func progressMessage(s pgprogress.Snapshot) string {
	if s.Found {
		return "migrator: migration in progress"
	}

	return "migrator: migration still running"
}

// progressAttrs composes the fields of one progress line.
//
// version, name and direction are the keys "migrator: migration applied"
// already uses, because an operator greps one set of keys and not two.
//
// elapsed is measured on this side and says how long this migration has been
// applying; statement_age comes from the server and says how long the current
// statement has been running. On a no-transaction migration of several
// statements the two diverge, which is why they are not one field.
func progressAttrs(
	mig Migration, d Direction, pid int, elapsed time.Duration, s pgprogress.Snapshot,
) []any {
	attrs := make([]any, 0, 24)
	attrs = append(attrs,
		"version", mig.Version,
		"name", mig.Name,
		"direction", d.String(),
		"pid", pid,
		"elapsed", elapsed.Round(time.Second))

	if !s.Found {
		attrs = append(attrs, "state", s.State)

		if wait := s.Wait(); wait != "" {
			attrs = append(attrs, "wait_event", wait)
		}

		return append(attrs, "statement_age", s.StatementAge.Round(time.Second))
	}

	attrs = append(attrs, "source", s.Source)
	attrs = appendIfSet(attrs, "command", s.Command)
	attrs = appendIfSet(attrs, "phase", s.Phase)
	attrs = appendIfSet(attrs, "relation", s.Relation)
	attrs = appendIfSet(attrs, "object", s.Object)

	if pct, ok := s.Percent(); ok {
		attrs = append(attrs, "percent", pct)
	}

	if s.Unit != "" {
		attrs = append(attrs, "progress", progressCount(s))
	}

	if s.Blocker != 0 {
		// An index build waiting for the transactions that were open when it
		// started. The pid is what somebody would take to pg_terminate_backend,
		// so it is the pid that is reported.
		attrs = append(attrs, "blocked_by", s.Blocker)
	}

	return attrs
}

// progressCount renders how far along the work is.
//
// Digits are grouped by the same rule as a row count in a plan: the difference
// between 12 400 000 and 1 240 000 is legible and the difference between
// 12400000 and 1240000 is not, and nobody counts digits at three in the
// morning.
func progressCount(s pgprogress.Snapshot) string {
	if s.Total <= 0 {
		return groupDigits(s.Done) + " " + s.Unit
	}

	return groupDigits(s.Done) + " of " + groupDigits(s.Total) + " " + s.Unit
}

// appendIfSet adds a key and its value, unless the value is empty.
func appendIfSet(attrs []any, key, value string) []any {
	if value == "" {
		return attrs
	}

	return append(attrs, key, value)
}
