package commands

import (
	"github.com/efureev/db-migrator/src/config"
)

func configCmd() *Command {
	cmd := NewCmd(`config`, `View config`, config.ViewConfig)

	return &cmd
}
