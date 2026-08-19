package pglock

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A Querier is the part of a database session [Enrich] needs.
//
// It is an interface rather than a concrete type so that this package stays
// free of the connection machinery: what it wants is one read of the
// catalogue, not a say in how the connection was made.
type Querier interface {
	// Query runs a query and reports its rows.
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Enrich fills in what only the server knows, in place.
//
// Two things: how large each relation is, so that "ACCESS EXCLUSIVE on orders"
// can become "ACCESS EXCLUSIVE on orders (~8 900 000 rows)", and which table an
// index belongs to. The second matters because DROP INDEX names only the index,
// while the lock everybody cares about is the one on its table.
//
// A relation that does not exist yet — created by an earlier statement of the
// same migration — is left alone rather than treated as an error: predicting a
// plan is not the same as validating it.
func Enrich(ctx context.Context, q Querier, preds []Prediction) error {
	names := distinctRelations(preds)
	if len(names) == 0 {
		return nil
	}

	// to_regclass returns NULL instead of raising for a name that resolves to
	// nothing, which is exactly the behaviour a predictor wants.
	rows, err := q.Query(ctx, `
		SELECT n.name,
		       coalesce(tc.oid, c.oid)::regclass::text AS relation,
		       coalesce(tc.reltuples, c.reltuples)::bigint AS rows
		  FROM unnest($1::text[]) AS n(name)
		  JOIN pg_class c ON c.oid = to_regclass(n.name)
		  LEFT JOIN pg_index i ON i.indexrelid = c.oid
		  LEFT JOIN pg_class tc ON tc.oid = i.indrelid`, names)
	if err != nil {
		return fmt.Errorf("pglock: read the catalogue: %w", err)
	}
	defer rows.Close()

	type found struct {
		relation string
		rows     int64
	}

	byName := make(map[string]found, len(names))

	for rows.Next() {
		var (
			name string
			f    found
		)

		if err := rows.Scan(&name, &f.relation, &f.rows); err != nil {
			return fmt.Errorf("pglock: read the catalogue: %w", err)
		}

		byName[name] = f
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("pglock: read the catalogue: %w", err)
	}

	for i := range preds {
		f, ok := byName[sanitise(preds[i].Relation)]
		if !ok {
			continue
		}

		if f.relation != "" {
			preds[i].Relation = unqualify(f.relation)
		}

		// reltuples is -1 when the relation has never been analysed, and this
		// package reports the same -1 for "not known" — the two really are the
		// same claim, and inventing a zero would read as an empty table.
		preds[i].Rows = f.rows
	}

	return nil
}

// ServerVersion reads server_version_num, which is what the version-dependent
// rules compare against.
func ServerVersion(ctx context.Context, q Querier) (int, error) {
	rows, err := q.Query(ctx, `SELECT current_setting('server_version_num')::int`)
	if err != nil {
		return 0, fmt.Errorf("pglock: read the server version: %w", err)
	}
	defer rows.Close()

	version, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[int])
	if err != nil {
		return 0, fmt.Errorf("pglock: read the server version: %w", err)
	}

	return version, nil
}

// distinctRelations reports the relations to look up, each sanitised into the
// form to_regclass parses.
func distinctRelations(preds []Prediction) []string {
	seen := make(map[string]bool, len(preds))
	out := make([]string, 0, len(preds))

	for _, p := range preds {
		if p.Relation == "" {
			continue
		}

		name := sanitise(p.Relation)
		if seen[name] {
			continue
		}

		seen[name] = true

		out = append(out, name)
	}

	return out
}

// sanitise quotes a relation name the way the server would need it written.
//
// The name arrives already folded — an unquoted identifier lower-cased, a
// quoted one as it was given — so quoting every part is both correct and
// necessary: a table created as "Users" is not the same as users.
func sanitise(name string) string {
	return pgx.Identifier(strings.Split(name, ".")).Sanitize()
}

// unqualify strips the quoting regclass adds back, so that the name printed in
// a plan looks like the one written in the migration.
func unqualify(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(p, `"`)
	}

	return strings.Join(parts, ".")
}
