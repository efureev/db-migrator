// Command migrator applies SQL migrations to PostgreSQL.
//
// Everything it does lives in internal/cli, which takes its streams as
// arguments; this file only picks the real ones. That is what makes the
// exit-code table testable a row at a time, and it is the opposite of version 1,
// where main reached into a global flag parser and the database layer called
// log.Fatal.
package main

import (
	"os"

	"github.com/efureev/db-migrator/v2/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Streams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}))
}
