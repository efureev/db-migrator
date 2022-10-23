package main

import (
	"github.com/efureev/db-migrator/src/commands"
	"github.com/efureev/db-migrator/src/config"
)

func main() {
	config.Init()
	commands.Init()

	commands.Usage()
}
