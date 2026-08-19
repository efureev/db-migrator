package migrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/efureev/db-migrator/v2/internal/pgprogress"
)

// audibleLogger is a logger that reports itself enabled, which is the thing
// progressFor asks about before it opens anything.
func audibleLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&strings.Builder{},
		&slog.HandlerOptions{Level: slog.LevelDebug}))
}

// attrString renders the key/value pairs of a log line for comparison.
func attrString(attrs []any) string {
	var b strings.Builder

	for i := 0; i+1 < len(attrs); i += 2 {
		if i > 0 {
			b.WriteByte(' ')
		}

		fmt.Fprintf(&b, "%v=%v", attrs[i], attrs[i+1])
	}

	return b.String()
}

// TestProgressOpensNothingWhenItWouldBeUnread passes a nil session on purpose.
//
// The claim is not that progressFor returns nil; it is that it decides so
// before it touches the database. A nil Session turns "it asked anyway" from a
// silent extra round trip into a panic, which is the only way to test the
// absence of a query.
func TestProgressOpensNothingWhenItWouldBeUnread(t *testing.T) {
	t.Parallel()

	t.Run("the interval is zero", func(t *testing.T) {
		t.Parallel()

		m := &Migrator{cfg: defaults()}
		m.cfg.logger = audibleLogger()
		m.cfg.progressInterval = 0

		if p := m.progressFor(t.Context(), nil); p != nil {
			t.Error("--progress-interval 0 still prepared a watcher")
		}
	})

	t.Run("no logger was given", func(t *testing.T) {
		t.Parallel()

		// The default logger discards everything. Polling would then be work
		// done to produce output nobody can see, over a connection opened to
		// carry it — which is what "enabled by default" must not cost a library
		// caller who never asked for a logger.
		m := &Migrator{cfg: defaults()}

		if p := m.progressFor(t.Context(), nil); p != nil {
			t.Error("a discarding logger still prepared a watcher")
		}
	})
}

// TestAbsentWatcherIsUsable pins the nil receiver. run and redo call watch and
// close unconditionally, and a watcher that was never prepared is the ordinary
// case — every offline run, every run with progress turned off.
func TestAbsentWatcherIsUsable(t *testing.T) {
	t.Parallel()

	var p *progressWatcher

	stop := p.watch(context.Background(), Migration{}, DirectionUp)
	stop()
	p.close(context.Background())
}

func TestProgressMessageSeparatesTheTwoEvents(t *testing.T) {
	t.Parallel()

	// A consumer of the JSON form filters on the message. "The index is 30%
	// built" and "it is alive and has said nothing for fourteen minutes" are
	// different things to be told, so they are different messages rather than
	// one message with a field to tell them apart.
	found := progressMessage(pgprogress.Snapshot{Found: true})
	heartbeat := progressMessage(pgprogress.Snapshot{})

	if found == heartbeat {
		t.Errorf("both events log the same message %q", found)
	}
}

func TestProgressAttrs(t *testing.T) {
	t.Parallel()

	mig := Migration{Version: 20260901130000, Name: "add_email_idx"}

	cases := []struct {
		name string
		snap pgprogress.Snapshot
		want string
	}{
		{
			name: "an index build reports the place and the distance",
			snap: pgprogress.Snapshot{
				State: "active", Found: true, Source: pgprogress.SourceCreateIndex,
				Command: "CREATE INDEX CONCURRENTLY", Phase: "building index",
				Relation: "users", Object: "users_email_idx",
				Unit: pgprogress.UnitBlocks, Done: 12400000, Total: 41200000,
			},
			want: "version=20260901130000 name=add_email_idx direction=up pid=4711 " +
				"elapsed=14m0s source=create_index command=CREATE INDEX CONCURRENTLY " +
				"phase=building index relation=users object=users_email_idx " +
				"percent=30 progress=12 400 000 of 41 200 000 blocks",
		},
		{
			// The ordinary state of an index build in its first phase, not an
			// exotic one: the server does not know the total yet. A percentage
			// would have to be invented, and 0% reads as "stuck".
			name: "the total is not known yet",
			snap: pgprogress.Snapshot{
				State: "active", Found: true, Source: pgprogress.SourceCreateIndex,
				Phase: "waiting for writers before build", Relation: "users",
				Unit: pgprogress.UnitLockers, Done: 3, Total: 0, Blocker: 8821,
			},
			want: "version=20260901130000 name=add_email_idx direction=up pid=4711 " +
				"elapsed=14m0s source=create_index phase=waiting for writers before build " +
				"relation=users progress=3 transactions blocked_by=8821",
		},
		{
			// The case the whole fallback exists for: a table rewrite, a
			// backfill or a lock wait, none of which any progress view knows
			// anything about.
			name: "nothing but a pulse, and it is waiting on a lock",
			snap: pgprogress.Snapshot{
				State: "active", WaitEventType: "Lock", WaitEvent: "relation",
				StatementAge: 14 * time.Minute,
			},
			want: "version=20260901130000 name=add_email_idx direction=up pid=4711 " +
				"elapsed=14m0s state=active wait_event=Lock:relation statement_age=14m0s",
		},
		{
			name: "a pulse that is not waiting for anything",
			snap: pgprogress.Snapshot{State: "active", StatementAge: 90 * time.Second},
			want: "version=20260901130000 name=add_email_idx direction=up pid=4711 " +
				"elapsed=14m0s state=active statement_age=1m30s",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got := attrString(progressAttrs(mig, DirectionUp, 4711, 14*time.Minute, c.snap))
			if got != c.want {
				t.Errorf("progressAttrs\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}
