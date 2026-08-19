//go:build integration

package migrator_test

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	migrator "github.com/efureev/db-migrator/v2"
	"github.com/efureev/db-migrator/v2/internal/testdb"
)

// logCapture collects slog records from whatever goroutine writes them.
//
// Two rules it exists to enforce. Everything is behind one mutex, because the
// watcher logs from a goroutine of its own while the test goroutine reads and
// every run of this suite is under -race. And nothing here touches *testing.T:
// a t.Logf from a goroutine that outlived its test panics the whole binary, and
// "the watcher outlived the run" is one of the things these tests are meant to
// detect. It has to fail as an assertion, not as a panic somewhere else.
type logCapture struct {
	*captureState

	pre []slog.Attr
}

type captureState struct {
	mu      sync.Mutex
	entries []logEntry
	sealed  bool
}

type logEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
	after bool // arrived after the run had returned
}

func newCapture() *logCapture { return &logCapture{captureState: &captureState{}} }

// logger reports a logger that keeps everything, debug included: half the
// assertions here are about the line that says why progress is unavailable.
func (c *logCapture) logger() *slog.Logger { return slog.New(c) }

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) WithGroup(string) slog.Handler { return c }

// WithAttrs keeps the attributes rather than dropping them. A handler that
// returned itself would make an assertion pass or fail depending on whether the
// library attached a field with logger.With or at the call site, which is a
// difference no test has any business seeing.
func (c *logCapture) WithAttrs(as []slog.Attr) slog.Handler {
	return &logCapture{captureState: c.captureState, pre: append(slices.Clip(c.pre), as...)}
}

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	e := logEntry{level: r.Level, msg: r.Message, attrs: map[string]string{}}

	for _, a := range c.pre {
		e.attrs[a.Key] = a.Value.String()
	}

	// Flattened here: a slog.Record must not be retained past Handle.
	r.Attrs(func(a slog.Attr) bool {
		e.attrs[a.Key] = a.Value.String()

		return true
	})

	c.mu.Lock()
	defer c.mu.Unlock()

	e.after = c.sealed
	c.entries = append(c.entries, e)

	return nil
}

// seal marks the point after which a line is a line too late.
func (c *logCapture) seal() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sealed = true
}

func (c *logCapture) filter(keep func(logEntry) bool) []logEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []logEntry

	for _, e := range c.entries {
		if keep(e) {
			out = append(out, e)
		}
	}

	return out
}

// progress reports the lines this feature emitted while a migration ran.
func (c *logCapture) progress() []logEntry {
	return c.filter(func(e logEntry) bool {
		return strings.HasPrefix(e.msg, "migrator: migration in progress") ||
			strings.HasPrefix(e.msg, "migrator: migration still running")
	})
}

// unavailable reports the lines explaining why nothing will be reported.
func (c *logCapture) unavailable() []logEntry {
	return c.filter(func(e logEntry) bool { return e.msg == "migrator: progress reporting is off" })
}

// slowIndexFixture builds a table whose index takes a known number of seconds
// to build on any machine.
//
// Sizing the table and hoping is the alternative, and it is not one: five
// million rows index in a moment on a warm laptop and in half a minute on a
// cold shared runner, so no row count is both quick enough for the suite and
// slow enough for CI. Here the duration is rows times sleep, a constant this
// test chooses, identical on every version and every machine.
func slowIndexFixture(t *testing.T, dsn string, rows int) {
	t.Helper()

	testdb.Exec(t, dsn, `CREATE TABLE big (id int)`)
	testdb.Exec(t, dsn, `INSERT INTO big SELECT generate_series(1, $1)`, rows)

	// IMMUTABLE is a lie here, and that is the point: PostgreSQL requires an
	// index expression to be immutable and checks only what the function
	// declares, never what it does. Declaring it VOLATILE — the truth — would
	// have CREATE INDEX reject it, and there would be no slow index build to
	// watch.
	testdb.Exec(t, dsn, `
		CREATE FUNCTION slow_id(i int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
		BEGIN
			PERFORM pg_sleep(0.1);
			RETURN i;
		END $$`)
}

// TestProgressReportsALongIndexBuild is the wired-up happy path.
//
// What makes it deterministic is that the observable window is four seconds of
// server-side sleeping that no machine can shorten, and that every assertion
// runs after Up has returned. It deliberately does not assert how many lines
// appeared, nor which phase the server was in, nor the wording: the first is
// timing, the second is a PostgreSQL implementation detail, and the third is a
// pure function tested without a database.
func TestProgressReportsALongIndexBuild(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	slowIndexFixture(t, dsn, 40) // 40 × 0.1s ≈ 4s

	capture := newCapture()

	m := newMigrator(t, dsn, map[string]string{
		"1_slow_index.up.sql": "CREATE INDEX big_slow_idx ON big (slow_id(id));",
	},
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(200*time.Millisecond))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	capture.seal()

	lines := capture.progress()
	if len(lines) == 0 {
		t.Fatalf("a four-second index build reported nothing; the lines that did appear: %v",
			capture.unavailable())
	}

	var seen bool

	for _, e := range lines {
		if e.attrs["source"] != "create_index" {
			continue
		}

		seen = true

		if e.attrs["phase"] == "" {
			t.Errorf("the line does not say which phase the build is in: %v", e.attrs)
		}

		if e.attrs["relation"] != "big" {
			t.Errorf("relation = %q, want %q", e.attrs["relation"], "big")
		}

		if e.attrs["version"] != "1" {
			t.Errorf("the line does not name the migration: %v", e.attrs)
		}
	}

	if !seen {
		t.Errorf("no line came from pg_stat_progress_create_index: %v", lines)
	}
}

// TestProgressFallsBackToAPulse covers the case the fallback exists for: a
// statement no progress view knows anything about. Most slow migrations are
// this case — a table rewrite, a backfill — and pg_sleep is the one that takes
// a known time.
func TestProgressFallsBackToAPulse(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	capture := newCapture()

	m := newMigrator(t, dsn, map[string]string{
		"1_slow.up.sql": "SELECT pg_sleep(2);",
	},
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(300*time.Millisecond))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	capture.seal()

	lines := capture.progress()
	if len(lines) == 0 {
		t.Fatalf("a two-second migration reported nothing; the lines that did appear: %v",
			capture.unavailable())
	}

	for _, e := range lines {
		if e.msg != "migrator: migration still running" {
			t.Errorf("a plain SELECT was reported as progress: %s %v", e.msg, e.attrs)
		}

		if e.attrs["state"] != "active" {
			t.Errorf("state = %q, want %q", e.attrs["state"], "active")
		}

		if e.attrs["statement_age"] == "" {
			t.Errorf("the pulse does not say how long the statement has been running: %v", e.attrs)
		}
	}
}

// TestProgressReportsALockWait is the other half of why the pulse exists: a
// migration that is not slow at all, only blocked. Nothing is timed here — the
// blocking lock is released after the wait has been observed, not after a sleep
// long enough to hope it was.
func TestProgressReportsALockWait(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	testdb.Exec(t, dsn, `CREATE TABLE blocked (id int)`)

	holder := testdb.Connect(t, dsn)

	tx, err := holder.Begin(ctx(t))
	if err != nil {
		t.Fatalf("begin the blocking transaction: %v", err)
	}

	if _, err := tx.Exec(ctx(t), `LOCK TABLE blocked IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("take the blocking lock: %v", err)
	}

	released := false

	defer func() {
		if !released {
			_ = tx.Rollback(context.WithoutCancel(ctx(t)))
		}
	}()

	capture := newCapture()

	m := newMigrator(t, dsn, map[string]string{
		"1_alter.up.sql": "ALTER TABLE blocked ADD COLUMN name text;",
	},
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(200*time.Millisecond),
		// The default is three seconds with retries, and the run would then
		// fail for a reason this test is not about.
		migrator.WithLockTimeout(30*time.Second))

	var (
		wg     sync.WaitGroup
		runErr error
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		_, runErr = m.Up(ctx(t))
	}()

	// Polling with a deadline rather than sleeping: the lock is held open until
	// the wait has been seen, so this waits for a condition the test controls
	// and not for a transient it hopes to catch.
	deadline := time.Now().Add(30 * time.Second)
	waiting := false

	for time.Now().Before(deadline) && !waiting {
		for _, e := range capture.progress() {
			if strings.HasPrefix(e.attrs["wait_event"], "Lock:") {
				waiting = true
			}
		}

		if !waiting {
			time.Sleep(50 * time.Millisecond)
		}
	}

	released = true

	if err := tx.Rollback(ctx(t)); err != nil {
		t.Errorf("release the blocking lock: %v", err)
	}

	wg.Wait()

	if runErr != nil {
		t.Fatalf("Up: %v", runErr)
	}

	if !waiting {
		t.Errorf("a migration blocked on a lock never said so: %v", capture.progress())
	}
}

// TestProgressIsOffWithFromConn is the executable form of a sentence in the
// documentation. FromConn hands back the connection its caller owns, so there
// is no second one to watch with — and a promise nobody checks is not a
// promise.
func TestProgressIsOffWithFromConn(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	capture := newCapture()

	m, err := migrator.New(migrator.FromConn(testdb.Connect(t, dsn)),
		src(map[string]string{"1_slow.up.sql": "SELECT pg_sleep(2);"}),
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	capture.seal()

	if lines := capture.progress(); len(lines) != 0 {
		t.Errorf("progress was reported over a single connection: %v", lines)
	}

	// Exactly one, not one per interval: over two seconds at 100ms a retrying
	// implementation would say it twenty times.
	off := capture.unavailable()
	if len(off) != 1 {
		t.Fatalf("the run explained itself %d times, want once: %v", len(off), off)
	}

	if !strings.Contains(off[0].attrs["reason"], "second connection") {
		t.Errorf("the reason does not name the cause: %v", off[0].attrs)
	}

	// And the run itself is untouched — which is the property that matters far
	// more than the reporting.
	if !testdb.TableExists(t, dsn, "public", "schema_migrations") {
		t.Error("the run did not complete")
	}
}

// TestProgressOverAPoolOfOne is the case where asking for a second session is
// not an error but a wait: pgxpool.Acquire blocks, and the connection it waits
// for is the one the run itself is holding. A watcher that acquired without a
// bound would deadlock — and a deadlock in a library a service calls at boot is
// not a degraded feature, it is a service that never starts.
func TestProgressOverAPoolOfOne(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	cfg.MaxConns = 1

	pool, err := pgxpool.NewWithConfig(ctx(t), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	t.Cleanup(pool.Close)

	capture := newCapture()

	m, err := migrator.New(migrator.FromPool(pool),
		src(map[string]string{"1_slow.up.sql": "SELECT pg_sleep(2);"}),
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Its own deadline, and not the two minutes the rest of the suite uses: a
	// deadlock should fail in half a minute with a name, not at the package
	// timeout with a goroutine dump.
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := m.Up(runCtx); err != nil {
		t.Fatalf("Up over a pool of one: %v", err)
	}

	if lines := capture.progress(); len(lines) != 0 {
		t.Errorf("progress was reported without a connection to report it over: %v", lines)
	}

	// The pool is usable afterwards. A watcher that left an Acquire pending
	// would take the connection the moment the run gave it back and never
	// return it, and this would hang rather than fail.
	c, err := pool.Acquire(runCtx)
	if err != nil {
		t.Fatalf("the pool did not come back: %v", err)
	}

	c.Release()
}

// TestProgressHandsThePooledConnectionBackAsItFoundIt: the watching connection
// gets a statement_timeout of its own so that a wedged catalogue query cannot
// sit there, and with FromPool that connection afterwards goes back to
// somebody's application.
func TestProgressHandsThePooledConnectionBackAsItFoundIt(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "250"
	cfg.MaxConns = 2

	pool, err := pgxpool.NewWithConfig(ctx(t), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	t.Cleanup(pool.Close)

	capture := newCapture()

	m, err := migrator.New(migrator.FromPool(pool),
		src(map[string]string{"1_slow.up.sql": "CREATE TABLE slow (id int);\nSELECT pg_sleep(0.6);"}),
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(200*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Both connections, because either could be the one the watcher used, and
	// both held at once: releasing each before taking the next would let the
	// pool hand back the same connection twice and prove half as much.
	held := make([]*pgxpool.Conn, 0, 2)

	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()

	for i := range 2 {
		c, err := pool.Acquire(ctx(t))
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		held = append(held, c)

		var timeout string
		if err := c.QueryRow(ctx(t), `SHOW statement_timeout`).Scan(&timeout); err != nil {
			t.Fatalf("SHOW statement_timeout: %v", err)
		}

		if timeout != "250ms" {
			t.Errorf("connection %d came back with statement_timeout %q, want %q", i, timeout, "250ms")
		}
	}
}

// soloRoleDSN reports a DSN for a role that may open exactly one connection to
// this database.
//
// The limit is on the role and not on the database because the test's own
// helper connections arrive as the superuser that created the container and
// must stay unaffected. The name is random because roles are cluster-wide while
// the database is the test's own: a fixed one collides with a parallel run and
// with the leftovers of a failed one.
func soloRoleDSN(t *testing.T, dsn string) string {
	t.Helper()

	database := databaseOf(t, dsn)
	role := "solo_" + strings.TrimPrefix(database, "mig_")

	testdb.Exec(t, dsn, `CREATE ROLE `+pgxIdent(role)+` LOGIN PASSWORD 'x' CONNECTION LIMIT 1`)
	testdb.Exec(t, dsn, `GRANT ALL ON SCHEMA public TO `+pgxIdent(role))
	// CREATE on the database as well as CONNECT: bootstrap issues CREATE SCHEMA
	// IF NOT EXISTS, and PostgreSQL checks the privilege before it checks
	// whether the schema is already there.
	testdb.Exec(t, dsn, `GRANT CONNECT, CREATE ON DATABASE `+pgxIdent(database)+` TO `+pgxIdent(role))

	t.Cleanup(func() {
		testdb.Exec(t, dsn, `DROP OWNED BY `+pgxIdent(role))
		testdb.Exec(t, dsn, `DROP ROLE IF EXISTS `+pgxIdent(role))
	})

	return strings.Replace(dsn, "migrator:migrator@", role+":x@", 1)
}

// TestProgressAttemptsNothingWhenItIsOff asks PostgreSQL to enforce the
// negative rather than inferring it from a census that only samples.
//
// The role may open one connection, which the run itself needs. With reporting
// off nothing is attempted, and the proof is that not even the line explaining
// a refusal appears.
func TestProgressAttemptsNothingWhenItIsOff(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	capture := newCapture()

	m := newMigrator(t, soloRoleDSN(t, dsn), twoTables,
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(0))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if lines := capture.progress(); len(lines) != 0 {
		t.Errorf("--progress-interval 0 reported progress: %v", lines)
	}

	if off := capture.unavailable(); len(off) != 0 {
		t.Errorf("--progress-interval 0 went looking for a connection: %v", off)
	}
}

// TestARefusedConnectionIsNotAFailedRun: the same role, and this time reporting
// is on. The server refuses the second connection, the run says so once and
// finishes anyway. Once and not once per interval — over two seconds at 100ms a
// retrying implementation would say it twenty times.
func TestARefusedConnectionIsNotAFailedRun(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	capture := newCapture()

	m := newMigrator(t, soloRoleDSN(t, dsn), map[string]string{
		"1_slow.up.sql": "SELECT pg_sleep(2);",
	},
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(100*time.Millisecond))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("a run whose watcher could not connect failed: %v", err)
	}

	capture.seal()

	if lines := capture.progress(); len(lines) != 0 {
		t.Errorf("progress was reported over a connection the server refused: %v", lines)
	}

	off := capture.unavailable()
	if len(off) != 1 {
		t.Fatalf("the run explained itself %d times, want once: %v", len(off), off)
	}
}

// TestAFastMigrationSaysNothing pins the first tick. A run that finishes inside
// one interval must produce no commentary at all, or every deploy that had
// nothing to do starts printing.
func TestAFastMigrationSaysNothing(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	capture := newCapture()

	m := newMigrator(t, dsn, twoTables,
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(30*time.Second))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	capture.seal()

	if lines := capture.progress(); len(lines) != 0 {
		t.Errorf("a run of two instant migrations reported progress: %v", lines)
	}
}

// TestTheWatchingConnectionIsHandedBack has a strong half and a weak one, and
// says which is which.
//
// The strong half is the server's: the backend either went away or it did not,
// and that is polled rather than looked at once, because a client closing a
// socket and a server reaping the backend are two events with a gap between
// them. The weak half is the log: "no line arrived after the run returned" can
// only produce a false pass, never a false failure. It is kept because it costs
// nothing, and named here so that nobody mistakes it for a proof.
func TestTheWatchingConnectionIsHandedBack(t *testing.T) {
	t.Parallel()

	dsn := testdb.Fresh(t)
	slowIndexFixture(t, dsn, 20)

	capture := newCapture()

	m := newMigrator(t, dsn, map[string]string{
		"1_slow_index.up.sql": "CREATE INDEX big_slow_idx ON big (slow_id(id));",
	},
		migrator.WithLogger(capture.logger()),
		migrator.WithProgressInterval(200*time.Millisecond))

	if _, err := m.Up(ctx(t)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	capture.seal()

	deadline := time.Now().Add(15 * time.Second)

	var left int

	for time.Now().Before(deadline) {
		left = testdb.QueryInt(t, dsn, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND pid <> pg_backend_pid()`)
		if left == 0 {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if left != 0 {
		t.Errorf("%d connection(s) to this test's database outlived the run", left)
	}

	for _, e := range capture.filter(func(e logEntry) bool { return e.after }) {
		t.Errorf("a line arrived after the run had returned: %s %v", e.msg, e.attrs)
	}
}
