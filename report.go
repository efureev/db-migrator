package migrator

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// A Report is the outcome of a run.
type Report struct {
	// Direction says which way the run went. A Redo reports DirectionDown: it
	// began by rolling back, and OneTransaction says how it ended.
	Direction Direction
	// Target is how far the run was asked to go.
	Target Target
	// StartedAt and Duration bound the whole run, lock wait included.
	StartedAt time.Time
	Duration  time.Duration
	// Applied is one record per migration the run wrote, in the order it wrote
	// them.
	Applied []Record
	// DryRun reports a run that decided what to do and did not do it.
	DryRun bool
	// OneTransaction reports whether the whole run held together. For Up and
	// Down it is true when the run completed; for Redo it is true only when
	// both halves shared one transaction, which a no-transaction migration
	// makes impossible.
	OneTransaction bool
	// Repairs is what a [Migrator.Repair] did. It is empty for every other run:
	// a repair edits bookkeeping and applies nothing, so it has no Applied.
	Repairs []RepairResult
}

// Len reports how many migrations the run touched.
func (r *Report) Len() int { return len(r.Applied) }

// Empty reports a run that did nothing, which is the normal outcome of Up
// against an up-to-date database.
func (r *Report) Empty() bool { return len(r.Applied) == 0 }

// Versions reports the versions the run touched, in order.
func (r *Report) Versions() []int64 {
	out := make([]int64, len(r.Applied))
	for i, rec := range r.Applied {
		out[i] = rec.Version
	}

	return out
}

// Current reports the highest version left in force by the run, or 0.
func (r *Report) Current() int64 {
	var current int64

	for _, rec := range r.Applied {
		if rec.InForce() && rec.Version > current {
			current = rec.Version
		}
	}

	return current
}

// String reports a one-line summary.
func (r *Report) String() string {
	if r.Empty() {
		return "nothing to do"
	}

	verb := "applied"
	if r.Direction == DirectionDown {
		verb = "rolled back"
	}

	return fmt.Sprintf("%d %s in %s", len(r.Applied), verb, r.Duration.Round(time.Millisecond))
}

// Text writes the report as a human reads it.
func (r *Report) Text(w io.Writer) error {
	if r.Empty() {
		_, err := fmt.Fprintln(w, "Schema is up to date. Nothing to apply.")

		return err
	}

	var b strings.Builder

	verb := "applied"
	if r.Direction == DirectionDown {
		verb = "reverted"
	}

	for _, rec := range r.Applied {
		state := verb
		if rec.RolledBackAt != nil {
			state = "reverted"
		} else if !rec.InForce() {
			state = "INCOMPLETE"
		}

		fmt.Fprintf(&b, "  %-10s %d_%s  %s\n",
			state, rec.Version, rec.Name, rec.ExecutionTime.Round(time.Millisecond))
	}

	fmt.Fprintf(&b, "\n  Done. %s.", r)

	if current := r.Current(); current > 0 {
		fmt.Fprintf(&b, " Current version %d.", current)
	}

	b.WriteString("\n")

	_, err := io.WriteString(w, b.String())

	return err
}

// JSON writes the report in the shape a CI step parses.
//
// An empty run writes an empty array rather than null, so that the shape of the
// output does not depend on the outcome.
func (r *Report) JSON(w io.Writer) error {
	type record struct {
		Version    int64  `json:"version"`
		Name       string `json:"name"`
		Checksum   string `json:"checksum"`
		AppliedAt  string `json:"applied_at"`
		DurationMS int64  `json:"duration_ms"`
		RolledBack bool   `json:"rolled_back"`
		Complete   bool   `json:"complete"`
	}

	out := struct {
		Direction      string   `json:"direction"`
		Target         string   `json:"target"`
		StartedAt      string   `json:"started_at"`
		DurationMS     int64    `json:"duration_ms"`
		DryRun         bool     `json:"dry_run"`
		OneTransaction bool     `json:"one_transaction"`
		Current        int64    `json:"current_version"`
		Applied        []record `json:"applied"`
	}{
		Direction:      r.Direction.String(),
		Target:         r.Target.String(),
		StartedAt:      r.StartedAt.UTC().Format(time.RFC3339),
		DurationMS:     r.Duration.Milliseconds(),
		DryRun:         r.DryRun,
		OneTransaction: r.OneTransaction,
		Current:        r.Current(),
		Applied:        make([]record, 0, len(r.Applied)),
	}

	for _, rec := range r.Applied {
		out.Applied = append(out.Applied, record{
			Version:    rec.Version,
			Name:       rec.Name,
			Checksum:   rec.Checksum,
			AppliedAt:  rec.AppliedAt.UTC().Format(time.RFC3339),
			DurationMS: rec.ExecutionTime.Milliseconds(),
			RolledBack: rec.RolledBackAt != nil,
			Complete:   rec.FinishedAt != nil,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// LogValue implements [slog.LogValuer], so that a caller can hand a whole run
// to its logger as one field instead of picking it apart.
func (r *Report) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("direction", r.Direction.String()),
		slog.Int("applied", len(r.Applied)),
		slog.Duration("duration", r.Duration),
		slog.Int64("current", r.Current()),
		slog.Bool("dry_run", r.DryRun),
	)
}

// Text writes the status as a table.
func (s *Status) Text(w io.Writer) error {
	var b strings.Builder

	if !s.Initialised {
		fmt.Fprintf(&b, "  The journal %q does not exist yet: nothing has been applied.\n", s.Schema+"."+s.Table)
		b.WriteString("  \"migrator up\" will create it.\n\n")
	}

	fmt.Fprintf(&b, "  %-16s %-28s %-12s %-21s %s\n",
		"VERSION", "NAME", "STATUS", "APPLIED AT", "TOOK")

	var applied, pending, drifted int

	for _, e := range s.Entries {
		at, took := "-", "-"

		if e.Record != nil {
			at = e.Record.AppliedAt.Local().Format("2006-01-02 15:04:05")
			if e.Record.ExecutionTime > 0 {
				took = e.Record.ExecutionTime.Round(time.Millisecond).String()
			}
		}

		fmt.Fprintf(&b, "  %-16d %-28s %-12s %-21s %s\n", e.Version, e.Name, e.State, at, took)

		switch {
		case e.State == StateApplied:
			applied++
		case e.State.NeedsAttention():
			drifted++
		default:
			pending++
		}
	}

	fmt.Fprintf(&b, "\n  %d applied, %d pending", applied, pending)

	if drifted > 0 {
		fmt.Fprintf(&b, ", %d needing attention", drifted)
	}

	if current := s.Current(); current > 0 {
		fmt.Fprintf(&b, ".  Current version %d.\n", current)
	} else {
		b.WriteString(".  No current version.\n")
	}

	if drifted > 0 {
		b.WriteString("  Run \"migrator validate\" for the details.\n")
	}

	_, err := io.WriteString(w, b.String())

	return err
}

// JSON writes the status in the shape a CI step parses.
func (s *Status) JSON(w io.Writer) error {
	type entry struct {
		Version   int64  `json:"version"`
		Name      string `json:"name"`
		State     string `json:"state"`
		AppliedAt string `json:"applied_at,omitempty"`
		Checksum  string `json:"checksum,omitempty"`
	}

	out := struct {
		Schema      string  `json:"schema"`
		Table       string  `json:"table"`
		Initialised bool    `json:"initialised"`
		Current     int64   `json:"current_version"`
		Pending     int     `json:"pending"`
		Entries     []entry `json:"entries"`
	}{
		Schema:      s.Schema,
		Table:       s.Table,
		Initialised: s.Initialised,
		Current:     s.Current(),
		Pending:     len(s.Pending()),
		Entries:     make([]entry, 0, len(s.Entries)),
	}

	for _, e := range s.Entries {
		item := entry{Version: e.Version, Name: e.Name, State: e.State.String()}

		if e.Record != nil {
			item.AppliedAt = e.Record.AppliedAt.UTC().Format(time.RFC3339)
			item.Checksum = e.Record.Checksum
		}

		out.Entries = append(out.Entries, item)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// String reports the version as one line, which is what "status --current" and
// a shell script both want.
func (v Version) String() string {
	if v.Current == 0 {
		return "no migrations applied"
	}

	s := strconv.FormatInt(v.Current, 10)
	if v.Name != "" {
		s += "_" + v.Name
	}

	if v.Dirty {
		s += " (incomplete migration on record)"
	}

	return s
}
