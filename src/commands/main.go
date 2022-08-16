package commands

import (
	"github.com/integrii/flaggy"
)

var ListCmd = []Commands{
	createCmd(),
	downCmd(),
	refreshCmd(),
	statusCmd(),
	upCmd(),
	versionCmd(),
	wipeCmd(),
}

var list []*Command

func Init() {
	flaggy.SetName("Migrator")
	flaggy.SetDescription("DB migrator")

	flaggy.DefaultParser.ShowHelpOnUnexpected = false

	getVersion()
	position := 1

	for _, cmd := range ListCmd {
		list = append(list, cmd.Get()...)
	}

	for _, cmd := range list {
		attach(cmd, position)
	}

	flaggy.Parse()
}

func attach(cmd *Command, position int) {
	flaggy.AttachSubcommand(cmd.Flaggy, position)
	if cmd.SubCommands != nil {
		for _, cmd := range cmd.SubCommands {
			attach(cmd, position+1)
		}
	}
}

func Usage() {
	find := false
	for _, cmd := range list {
		if cmd.Flaggy.Used {
			cmd.Usage()
			find = true
		}
	}

	if !find {
		flaggy.ShowHelpAndExit(`Command not found!`)
	}
}
