package main

import (
	"migrator/src/commands"
	"migrator/src/config"
)

func main() {
	config.Init()
	commands.Init()

	commands.Usage()
}
