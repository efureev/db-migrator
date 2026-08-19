// Package pgprogress reads what one PostgreSQL backend is doing right now.
//
// PostgreSQL reports the progress of its own long operations and almost nobody
// reads it. Five views cover index builds, VACUUM, ANALYZE, CLUSTER and COPY.
// Everything else — an ALTER TABLE that rewrites the table, a backfill UPDATE,
// a wait on a lock — appears in none of them, which is why pg_stat_activity is
// read too, and not as a fallback of last resort: for the majority of slow
// migrations it is the only answer there is.
//
// The package reads. It never writes, and it never decides anything: composing
// the sentence a person reads belongs to whoever has the migration in hand.
package pgprogress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A Querier is the part of a database session this package needs.
//
// QueryRow rather than Query: every question here has exactly one row for an
// answer, and there is no rows.Close to forget.
type Querier interface {
	// QueryRow runs a query and reports its single row.
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// The progress views, in the order [buildQuery] expects to hear about them.
const (
	viewCreateIndex = iota
	viewVacuum
	viewAnalyze
	viewCluster
	viewCopy
	viewCount
)

// The values of [Snapshot.Source], which name the view a snapshot came from.
const (
	SourceCreateIndex = "create_index"
	SourceVacuum      = "vacuum"
	SourceAnalyze     = "analyze"
	SourceCluster     = "cluster"
	SourceCopy        = "copy"
)

// The values of [Snapshot.Unit], which say what Done and Total count.
//
// Which one is meaningful depends on the phase, and the choice is made by the
// server-side query: while an index build waits for writers, blocks_total is
// zero and the number that moves is the count of transactions still to finish.
// Reporting "0 of 0 blocks" for twenty minutes is how a working index build
// comes to read as a hung one.
const (
	UnitBlocks  = "blocks"
	UnitLockers = "transactions"
	UnitTuples  = "rows"
	UnitBytes   = "bytes"
)

// A Snapshot is one backend at one moment.
//
// The pg_stat_activity half is always filled in for a backend that exists. The
// progress half is filled in only when one of the progress views had a row for
// it, which [Snapshot.Found] reports.
type Snapshot struct {
	// State is pg_stat_activity.state: "active", "idle in transaction" and the
	// rest. It is empty when the monitoring role may not see this backend.
	State string
	// WaitEventType and WaitEvent are what the backend is waiting for, both
	// empty when it is not waiting.
	WaitEventType, WaitEvent string
	// StatementAge is how long the current statement has been running.
	StatementAge time.Duration

	// Found reports that a progress view had a row for this backend, and
	// therefore that the fields below mean anything.
	Found bool
	// Source names the view, as one of the Source constants.
	Source string
	// Command is the command the view reports, where it reports one: "CREATE
	// INDEX CONCURRENTLY", "VACUUM FULL", "COPY FROM".
	Command string
	// Phase is the phase within that command, in the server's own words.
	Phase string
	// Relation is the table being worked on, schema-qualified when it is not on
	// the search path, and empty when the view reports no table — COPY (SELECT
	// ...) TO has none, and that is normal.
	Relation string
	// Object is the secondary object the work is about: the index being built.
	Object string
	// Unit says what Done and Total count, as one of the Unit constants.
	Unit string
	// Done and Total are how far along the operation is. Total is 0 when the
	// server does not know it yet, which is the ordinary state of an index
	// build in its first phase rather than a failure.
	Done, Total int64
	// Blocker is the pid this operation is waiting for, or 0. Only an index
	// build reports one.
	Blocker int
}

// Percent reports how far along the operation is, and whether that is known.
//
// It can exceed 100: a partitioned index build counts the whole set of
// partitions in Total and each partition's blocks in Done. Reporting that
// honestly is better than clamping it and leaving somebody to wonder why the
// number stopped moving.
func (s Snapshot) Percent() (int, bool) {
	if s.Total <= 0 || s.Done < 0 {
		return 0, false
	}

	return int(float64(s.Done) / float64(s.Total) * 100), true
}

// Wait reports the wait the way PostgreSQL's documentation spells it,
// "Lock:relation", or "" when the backend is not waiting on anything.
func (s Snapshot) Wait() string {
	switch {
	case s.WaitEventType == "":
		return ""
	case s.WaitEvent == "":
		return s.WaitEventType
	default:
		return s.WaitEventType + ":" + s.WaitEvent
	}
}

// Restricted reports a backend whose row is there but blanked out.
//
// That is what a role without pg_read_all_stats sees of a session belonging to
// another role: the row exists, and every column worth reading is NULL. It is
// not a transient condition, so there is no point asking again.
func (s Snapshot) Restricted() bool { return s.State == "" }

// A Reader polls one backend through a query built for one server.
type Reader struct{ query string }

// New builds a Reader over the progress views this server actually has.
//
// Which views exist is asked of the server rather than worked out from its
// version. It is one round trip, it is direct evidence, and it means a query
// naming a view that is not there — 42P01, fatal to the whole UNION and not to
// one branch of it — cannot be built in the first place. For the record:
// pg_stat_progress_create_index and _cluster arrived in PostgreSQL 12,
// _analyze in 13, _copy in 14.
func New(ctx context.Context, q Querier) (*Reader, error) {
	var present [viewCount]bool

	err := q.QueryRow(ctx, `
		SELECT to_regclass('pg_catalog.pg_stat_progress_create_index') IS NOT NULL,
		       to_regclass('pg_catalog.pg_stat_progress_vacuum')       IS NOT NULL,
		       to_regclass('pg_catalog.pg_stat_progress_analyze')      IS NOT NULL,
		       to_regclass('pg_catalog.pg_stat_progress_cluster')      IS NOT NULL,
		       to_regclass('pg_catalog.pg_stat_progress_copy')         IS NOT NULL`).
		Scan(&present[viewCreateIndex], &present[viewVacuum], &present[viewAnalyze],
			&present[viewCluster], &present[viewCopy])
	if err != nil {
		return nil, fmt.Errorf("pgprogress: ask which progress views this server has: %w", err)
	}

	return &Reader{query: buildQuery(present)}, nil
}

// Read reports what the backend with the given pid is doing.
//
// It reports ok=false when pg_stat_activity has no row for the pid at all,
// which means the backend is gone — the ordinary way a poll ends.
func (r *Reader) Read(ctx context.Context, q Querier, pid int) (Snapshot, bool, error) {
	var (
		s       Snapshot
		seconds float64
	)

	err := q.QueryRow(ctx, r.query, pid).Scan(
		&s.State, &s.WaitEventType, &s.WaitEvent, &seconds,
		&s.Found, &s.Source, &s.Command, &s.Phase, &s.Relation, &s.Object,
		&s.Unit, &s.Done, &s.Total, &s.Blocker)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Snapshot{}, false, nil
	case err != nil:
		return Snapshot{}, false, fmt.Errorf("pgprogress: read backend %d: %w", pid, err)
	}

	s.StatementAge = time.Duration(seconds * float64(time.Second))

	return s, true, nil
}

// activityColumns is the half of the answer that is always available.
//
// clock_timestamp() and not now(): now() is the transaction's start time, and
// a caller who ever wraps a poll in a transaction would then watch the age of
// the statement stay the same forever.
const activityColumns = `coalesce(a.state, ''),
		       coalesce(a.wait_event_type, ''),
		       coalesce(a.wait_event, ''),
		       coalesce(extract(epoch FROM clock_timestamp() - a.query_start), 0)::float8`

// buildQuery composes the one query that answers everything about one backend.
//
// One query and not six. The activity row is wanted on every poll, not only
// when no progress view has anything, so asking for it separately would be a
// second round trip every time. The alternative — read activity, then dispatch
// on the text of a.query — would mean parsing somebody else's SQL to decide
// which catalogue to read, and this project does not guess at SQL it did not
// write.
//
// Every branch is normalised to the same nine columns and every column is cast
// explicitly, because a UNION takes its types from its first branch and which
// branch is first depends on what this server has.
func buildQuery(present [viewCount]bool) string {
	branches := make([]string, 0, viewCount)

	if present[viewCreateIndex] {
		// The counter that means anything moves with the phase, so the choice
		// is made here where all three are visible at once.
		branches = append(branches, `
		SELECT 'create_index'::text, v.command::text, v.phase::text,
		       CASE WHEN v.relid <> 0 THEN v.relid::regclass::text ELSE '' END::text,
		       CASE WHEN v.index_relid <> 0 THEN v.index_relid::regclass::text ELSE '' END::text,
		       CASE WHEN v.blocks_total  > 0 THEN 'blocks'
		            WHEN v.lockers_total > 0 THEN 'transactions'
		            WHEN v.tuples_total  > 0 THEN 'rows'
		            ELSE '' END::text,
		       CASE WHEN v.blocks_total  > 0 THEN v.blocks_done
		            WHEN v.lockers_total > 0 THEN v.lockers_done
		            WHEN v.tuples_total  > 0 THEN v.tuples_done
		            ELSE 0 END::bigint,
		       CASE WHEN v.blocks_total  > 0 THEN v.blocks_total
		            WHEN v.lockers_total > 0 THEN v.lockers_total
		            WHEN v.tuples_total  > 0 THEN v.tuples_total
		            ELSE 0 END::bigint,
		       coalesce(v.current_locker_pid, 0)::int
		  FROM pg_catalog.pg_stat_progress_create_index v WHERE v.pid = a.pid`)
	}

	if present[viewVacuum] {
		branches = append(branches, `
		SELECT 'vacuum'::text, ''::text, v.phase::text,
		       CASE WHEN v.relid <> 0 THEN v.relid::regclass::text ELSE '' END::text,
		       ''::text, 'blocks'::text,
		       v.heap_blks_scanned::bigint, v.heap_blks_total::bigint, 0::int
		  FROM pg_catalog.pg_stat_progress_vacuum v WHERE v.pid = a.pid`)
	}

	if present[viewAnalyze] {
		branches = append(branches, `
		SELECT 'analyze'::text, ''::text, v.phase::text,
		       CASE WHEN v.relid <> 0 THEN v.relid::regclass::text ELSE '' END::text,
		       ''::text, 'blocks'::text,
		       v.sample_blks_scanned::bigint, v.sample_blks_total::bigint, 0::int
		  FROM pg_catalog.pg_stat_progress_analyze v WHERE v.pid = a.pid`)
	}

	if present[viewCluster] {
		branches = append(branches, `
		SELECT 'cluster'::text, v.command::text, v.phase::text,
		       CASE WHEN v.relid <> 0 THEN v.relid::regclass::text ELSE '' END::text,
		       ''::text,
		       CASE WHEN v.heap_blks_total > 0 THEN 'blocks' ELSE 'rows' END::text,
		       CASE WHEN v.heap_blks_total > 0 THEN v.heap_blks_scanned
		            ELSE v.heap_tuples_scanned END::bigint,
		       v.heap_blks_total::bigint, 0::int
		  FROM pg_catalog.pg_stat_progress_cluster v WHERE v.pid = a.pid`)
	}

	if present[viewCopy] {
		// COPY reports no phase, and COPY (SELECT ...) TO reports no relation.
		// Both are ordinary, so neither is worth a column that says "unknown".
		branches = append(branches, `
		SELECT 'copy'::text, v.command::text, ''::text,
		       CASE WHEN v.relid <> 0 THEN v.relid::regclass::text ELSE '' END::text,
		       ''::text,
		       CASE WHEN v.bytes_total > 0 THEN 'bytes' ELSE 'rows' END::text,
		       CASE WHEN v.bytes_total > 0 THEN v.bytes_processed
		            ELSE v.tuples_processed END::bigint,
		       v.bytes_total::bigint, 0::int
		  FROM pg_catalog.pg_stat_progress_copy v WHERE v.pid = a.pid`)
	}

	// A server with no progress views at all is not one this tool supports, but
	// answering with the activity half rather than failing keeps the shape of
	// the answer — and the single Scan that reads it — the same either way.
	if len(branches) == 0 {
		return `
		SELECT ` + activityColumns + `,
		       false, ''::text, ''::text, ''::text, ''::text, ''::text, ''::text,
		       0::bigint, 0::bigint, 0::int
		  FROM pg_catalog.pg_stat_activity a
		 WHERE a.pid = $1`
	}

	// LIMIT 1 because a backend runs one command at a time — a guarantee worth
	// having in the query rather than in an assumption about the server.
	return `
		SELECT ` + activityColumns + `,
		       p.source IS NOT NULL,
		       coalesce(p.source, ''), coalesce(p.command, ''), coalesce(p.phase, ''),
		       coalesce(p.relation, ''), coalesce(p.object, ''), coalesce(p.unit, ''),
		       coalesce(p.done, 0), coalesce(p.total, 0), coalesce(p.blocker, 0)
		  FROM pg_catalog.pg_stat_activity a
		  LEFT JOIN LATERAL (
		    SELECT * FROM (` + strings.Join(branches, `
		    UNION ALL`) + `
		    ) u (source, command, phase, relation, object, unit, done, total, blocker)
		    LIMIT 1
		  ) p ON true
		 WHERE a.pid = $1`
}
