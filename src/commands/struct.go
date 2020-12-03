package commands

import (
	"github.com/integrii/flaggy"
)

type Commands interface {
	Get() []*Command
}

type Command struct {
	Name        string
	Flaggy      *flaggy.Subcommand
	Usage       func()
	SubCommands []*Command
}

func (cmd *Command) Get() []*Command {
	return []*Command{cmd}
}

type CommandGroup struct {
	Name     string
	Commands []*Command
}

func (g *CommandGroup) Get() []*Command {
	for _, cmd := range g.Commands {
		cmd.Name = g.Name + `:` + cmd.Name
		cmd.Flaggy.Name = cmd.Name
	}

	return g.Commands
}

func NewCmdGroup(name string, cmds []*Command) CommandGroup {
	return CommandGroup{
		Name:     name,
		Commands: cmds,
	}
}

func NewCmd(name, desc string, usage func()) Command {
	return Command{
		Name:   name,
		Flaggy: NewCmdFlaggy(name, desc),
		Usage:  usage,
	}
}

func NewCmdFlaggy(name, desc string) *flaggy.Subcommand {
	cmd := flaggy.NewSubcommand(name)
	cmd.Description = desc

	return cmd
}
