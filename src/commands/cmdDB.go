package commands

import (
	"migrator/src/db"
)

var migrationNameFile string

func createCmd() *Command {
	cmd := NewCmd(`create`, `Create migration`, db.MigrateCreate(&migrationNameFile))
	cmd.Flaggy.String(&migrationNameFile, "n", "name", "Name of new migration")
	cmd.Flaggy.AdditionalHelpPrepend = `Example: ./migrator create --name='create social users'`
	return &cmd
}

func upCmd() *Command {
	cmd := NewCmd(`up`, `Migrate up`, db.MigrateUp)

	return &cmd
}

func downCmd() *Command {
	cmd := NewCmd(`down`, `Migrate down`, db.MigrateDown)

	return &cmd
}

func refreshCmd() *Command {
	cmd := NewCmd(`fresh`, `Refresh all migrations`, db.MigrateRefresh)

	return &cmd
}

func wipeCmd() *Command {
	cmd := NewCmd(`wipe`, `Wipe all data & structures in DB`, db.Wipe)

	return &cmd
}

func versionCmd() *Command {
	cmd := NewCmd(`version`, `Print current migration version`, db.Version)

	return &cmd
}
