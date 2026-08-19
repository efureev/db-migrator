package migrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// A Holder is one session holding this Migrator's advisory lock.
type Holder struct {
	// PID is the backend process id, which is what pg_terminate_backend takes.
	PID int
	// User, Application and Client say who it is, as far as PostgreSQL knows.
	// Application is empty unless the client set application_name.
	User        string
	Application string
	Client      string
	// Database is the database the holder connected to. Advisory locks are
	// cluster-wide, so a run in a different database can hold the lock this one
	// is waiting for — which is confusing enough to be worth printing.
	Database string
	// State is what the backend is doing: "active", "idle in transaction", …
	State string
	// Running is how long its current query has been going.
	Running time.Duration
	// Query is that query, as pg_stat_activity reports it.
	Query string
}

// String reports the holder as one line.
func (h Holder) String() string {
	who := h.User
	if h.Application != "" {
		who += " (" + h.Application + ")"
	}

	return fmt.Sprintf("pid %d  %s  on %s  %s for %s",
		h.PID, who, h.Database, h.State, h.Running.Round(time.Second))
}

// A LockReport is who holds this Migrator's advisory lock.
type LockReport struct {
	// ClassID and ObjID identify the lock, so that an operator can search for
	// it themselves.
	ClassID int32
	ObjID   int32
	// Schema and Table are what the identifier was derived from.
	Schema string
	Table  string
	// Holders is who has it. Empty means nobody does.
	Holders []Holder
}

// Held reports whether anybody holds the lock.
func (r *LockReport) Held() bool { return len(r.Holders) > 0 }

// String reports a one-line summary.
func (r *LockReport) String() string {
	if !r.Held() {
		return fmt.Sprintf("advisory lock %d/%d is free", r.ClassID, r.ObjID)
	}

	return fmt.Sprintf("advisory lock %d/%d is held by %d session(s)",
		r.ClassID, r.ObjID, len(r.Holders))
}

// Text writes the report as a person reads it.
func (r *LockReport) Text(w io.Writer) error {
	var b strings.Builder

	fmt.Fprintf(&b, "  Lock     %d/%d  (derived from %s.%s)\n", r.ClassID, r.ObjID, r.Schema, r.Table)

	if !r.Held() {
		b.WriteString("  Nobody is holding it.\n")

		_, err := io.WriteString(w, b.String())

		return err
	}

	for _, h := range r.Holders {
		fmt.Fprintf(&b, "\n  %s\n", h)

		if h.Query != "" {
			fmt.Fprintf(&b, "    %s\n", firstLine(h.Query))
		}
	}

	fmt.Fprintf(&b, "\n  To end one:  SELECT pg_terminate_backend(%d);\n", r.Holders[0].PID)

	_, err := io.WriteString(w, b.String())

	return err
}

// JSON writes the report in the shape a CI step parses.
func (r *LockReport) JSON(w io.Writer) error {
	type holder struct {
		PID         int    `json:"pid"`
		User        string `json:"user"`
		Application string `json:"application,omitempty"`
		Client      string `json:"client,omitempty"`
		Database    string `json:"database"`
		State       string `json:"state"`
		RunningMS   int64  `json:"running_ms"`
		Query       string `json:"query,omitempty"`
	}

	out := struct {
		Format  int      `json:"format"`
		ClassID int32    `json:"class_id"`
		ObjID   int32    `json:"obj_id"`
		Schema  string   `json:"schema"`
		Table   string   `json:"table"`
		Held    bool     `json:"held"`
		Holders []holder `json:"holders"`
	}{
		Format: JSONFormat, ClassID: r.ClassID, ObjID: r.ObjID,
		Schema: r.Schema, Table: r.Table, Held: r.Held(),
		Holders: make([]holder, 0, len(r.Holders)),
	}

	for _, h := range r.Holders {
		out.Holders = append(out.Holders, holder{
			PID: h.PID, User: h.User, Application: h.Application, Client: h.Client,
			Database: h.Database, State: h.State,
			RunningMS: h.Running.Milliseconds(), Query: h.Query,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// Locks reports who holds the advisory lock this Migrator serialises on.
//
// It exists because "another migration run holds the lock" is a true answer
// that leads nowhere: the next question is always which one, and answering it
// otherwise means remembering that the identifier is derived from the schema
// and table names and then writing the join by hand, at the hour when that is
// hardest.
//
// It takes no lock and writes nothing, so it is safe to run while a deploy is
// stuck — which is the only time anybody runs it.
func (m *Migrator) Locks(ctx context.Context) (*LockReport, error) {
	classID, objID := m.lockID()

	report := &LockReport{
		ClassID: classID, ObjID: objID,
		Schema: m.cfg.schema, Table: m.cfg.table,
	}

	s, err := m.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	defer func() { s.Release(context.WithoutCancel(ctx)) }()

	// Not filtered by database: advisory locks are cluster-wide, and a holder
	// in another database is exactly the case somebody would otherwise spend
	// twenty minutes failing to explain.
	rows, err := s.Query(ctx, `
		SELECT l.pid,
		       coalesce(a.usename::text, ''),
		       coalesce(a.application_name, ''),
		       coalesce(host(a.client_addr), ''),
		       coalesce(a.datname, ''),
		       coalesce(a.state, ''),
		       coalesce(extract(epoch FROM now() - a.query_start), 0)::float8,
		       coalesce(a.query, '')
		  FROM pg_locks l
		  LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		 WHERE l.locktype = 'advisory' AND l.classid = $1 AND l.objid = $2
		   AND l.granted
		 ORDER BY l.pid`, classID, objID)
	if err != nil {
		return nil, fmt.Errorf("migrator: read the advisory lock holders: %w", redact(err))
	}
	defer rows.Close()

	for rows.Next() {
		var (
			h       Holder
			seconds float64
		)

		if err := rows.Scan(&h.PID, &h.User, &h.Application, &h.Client,
			&h.Database, &h.State, &seconds, &h.Query); err != nil {
			return nil, fmt.Errorf("migrator: read the advisory lock holders: %w", redact(err))
		}

		h.Running = time.Duration(seconds * float64(time.Second))
		report.Holders = append(report.Holders, h)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrator: read the advisory lock holders: %w", redact(err))
	}

	return report, nil
}

// firstLine reports the first line of s, trimmed, for a query printed inside a
// table.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")

	const limit = 100
	if len(line) > limit {
		return line[:limit] + "…"
	}

	return line
}
