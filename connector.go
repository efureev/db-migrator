package migrator

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Conn is the pgx surface a run uses.
//
// It is satisfied by *pgx.Conn, *pgxpool.Conn and pgx.Tx alike, which is
// exactly why it is not the abstraction the constructor takes — see [Session].
type Conn interface {
	// Exec runs a statement that returns no rows.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	// Query runs a statement that returns rows.
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	// QueryRow runs a statement that returns at most one row.
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	// Begin opens a transaction.
	Begin(ctx context.Context) (pgx.Tx, error)
}

// A Session is one pinned PostgreSQL backend, held for a whole run.
//
// Pinning is not an optimisation. The advisory lock that serialises concurrent
// runs is session-level: taken on one backend and released on another, it is
// not released at all. SET is session-level too, so the timeouts a migration
// needs never reach the statement that needed them. A *pgxpool.Pool satisfies
// [Conn] and would compile — and would then route consecutive statements to
// different backends, failing once in a hundred deploys in a way nobody can
// reproduce. Hence this type.
type Session interface {
	Conn

	// Release hands the session back to whoever owns it. It is called exactly
	// once, at the end of a run, on a context that is never already cancelled.
	Release(ctx context.Context)
}

// A Connector hands a run one pinned session.
type Connector interface {
	// Acquire reports a session held for the whole of one run.
	Acquire(ctx context.Context) (Session, error)
}

// FromDSN opens a connection per run and closes it afterwards.
//
// This is what the CLI uses: a migration wants a session of its own, not one
// shaped by an application's pool settings.
//
// An empty dsn is not an error. pgx then reads PGHOST, PGPORT, PGUSER,
// PGPASSWORD, PGDATABASE, PGSSLMODE, PGSERVICE, PGPASSFILE and ~/.pgpass, which
// means an unconfigured migrator connects exactly where psql with no arguments
// connects. That is a feature worth having and costs nothing to keep.
func FromDSN(dsn string) Connector { return dsnConnector{dsn: dsn} }

// FromConn wraps a connection the caller owns.
//
// Release neither closes nor resets the connection; only the settings the run
// itself changed are put back.
func FromConn(conn *pgx.Conn) Connector { return connConnector{conn: conn} }

// FromPool takes one connection out of the pool for each run and returns it
// afterwards.
//
// The connection is held for the whole run, which is what makes a pool usable
// here at all. Every GUC the run sets is reset before the connection goes back,
// so a migration that legitimately runs for an hour does not leave its
// statement_timeout behind on a pooled connection.
func FromPool(pool *pgxpool.Pool) Connector { return poolConnector{pool: pool} }

type dsnConnector struct{ dsn string }

func (c dsnConnector) Acquire(ctx context.Context) (Session, error) {
	cfg, err := pgx.ParseConfig(c.dsn)
	if err != nil {
		// The DSN is not echoed: the usual reason it fails to parse is a
		// password holding a character that needed escaping, and repeating it
		// "for debugging" leaks exactly the secret worth hiding.
		return nil, fmt.Errorf("migrator: connection string is not valid: %w", redact(err))
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("migrator: connect: %w", redact(err))
	}

	return &ownedSession{Conn: conn}, nil
}

type connConnector struct{ conn *pgx.Conn }

func (c connConnector) Acquire(context.Context) (Session, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("migrator: %w", errNilConn)
	}

	return borrowedSession{Conn: c.conn}, nil
}

type poolConnector struct{ pool *pgxpool.Pool }

func (c poolConnector) Acquire(ctx context.Context) (Session, error) {
	if c.pool == nil {
		return nil, fmt.Errorf("migrator: %w", errNilPool)
	}

	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrator: acquire from pool: %w", redact(err))
	}

	return &pooledSession{Conn: conn.Conn(), handle: conn}, nil
}

// ownedSession is a connection this package opened and therefore closes.
type ownedSession struct{ *pgx.Conn }

func (s *ownedSession) Release(ctx context.Context) { _ = s.Close(ctx) }

// borrowedSession is a connection the caller owns. Closing it is not ours to
// do; the run resets its own settings before returning.
type borrowedSession struct{ *pgx.Conn }

func (borrowedSession) Release(context.Context) {}

// pooledSession is one connection held out of a pool for the whole run.
type pooledSession struct {
	*pgx.Conn

	handle *pgxpool.Conn
}

func (s *pooledSession) Release(context.Context) { s.handle.Release() }
