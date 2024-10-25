package main

import (
	"github.com/efureev/db-migrator/src/commands"
	"github.com/efureev/db-migrator/src/config"
)

func main() {
	flags := config.ParseFlag()
	config.GetConfig(flags.ConfigFilePath)

	commands.Init()

	commands.Usage()
}
