// Command embed shows the main way a service uses this as a library: the
// migrations travel inside the binary, and the service applies them at startup.
//
// It is a runnable example rather than a snippet in the README because a
// snippet that does not compile is worse than no snippet.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	migrator "github.com/efureev/db-migrator/v2"
)

//go:embed migrations/*.sql
var embedded embed.FS

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(log); err != nil {
		log.Error("migrations failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// An empty DSN is fine: pgx reads PGHOST, PGUSER and the rest, so this
	// connects where psql with no arguments connects.
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	// The embedded FS is rooted above the files; fs.Sub points at the directory
	// that holds them, which is what Load expects.
	src, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return err
	}

	// No WithAllowDown and no WithAllowWipe: a service has no business rolling
	// back or erasing its own schema, and leaving them out makes that
	// impossible rather than merely discouraged.
	m, err := migrator.New(migrator.FromPool(pool), src,
		migrator.WithLogger(log),
		migrator.WithMigratorTag("example/1.0"),
	)
	if err != nil {
		return err
	}

	report, err := m.Up(ctx)
	if err != nil {
		return err
	}

	log.Info("migrations applied", "report", report)

	return nil
}
