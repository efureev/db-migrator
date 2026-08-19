// Package cli is the command layer of the migrator binary.
//
// Everything testable lives here; cmd/migrator is thirty lines that pick the
// real streams and call [Run]. That split is what lets the exit-code table have
// a test per row.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	migrator "github.com/efureev/db-migrator/v2"
	"github.com/efureev/db-migrator/v2/internal/buildinfo"
	"github.com/efureev/db-migrator/v2/internal/config"
	"github.com/efureev/db-migrator/v2/internal/ui"
)

// Streams are the three files a command talks to.
type Streams struct {
	In       io.Reader
	Out, Err io.Writer
}

// A command is one subcommand.
type command struct {
	name    string
	aliases []string
	summary string
	// needsDB marks a command that opens a connection. It is enforced, not
	// documentation: [env.open] refuses to run for a command that declared it
	// does not need a database, so "create works on a plane" cannot quietly
	// stop being true.
	needsDB bool
	run     func(*env, []string) error
	usage   func(io.Writer)
}

// env is everything a subcommand needs.
type env struct {
	cfg     *config.Config
	ui      *ui.UI
	streams Streams
	ctx     context.Context
	cmd     command
}

// Run parses args, runs the command they name and reports the exit code.
func Run(args []string, streams Streams) int {
	ctx, stop := signalContext(context.Background(), streams.Err)
	defer stop()

	code, err := run(ctx, args, streams)
	if err == nil {
		return code
	}

	// The UI may not exist yet when the failure is in parsing, so the fallback
	// writes plainly rather than not at all.
	fmt.Fprintln(streams.Err, ui.Prefixed(err.Error()))

	return ExitCode(err)
}

func run(ctx context.Context, args []string, streams Streams) (int, error) {
	name, rest, flags, err := split(args)
	if err != nil {
		return 0, err
	}

	if name == "" {
		usage(streams.Out)

		return ExitUsage, nil
	}

	cmd, ok := lookup(name)
	if !ok {
		usage(streams.Err)

		return 0, usageErrorf("unknown command %q", name)
	}

	cfg, err := config.Load(splitList(flags["env-file"]), flags)
	if err != nil {
		// A configuration the tool cannot accept is a usage error: the fix is
		// always in the invocation or the environment, never in the database,
		// and exit code 2 is what tells a CI step not to retry.
		return 0, joinUsage(err)
	}

	level, err := ui.ParseLevel(cfg.LogLevel)
	if err != nil {
		return 0, joinUsage(err)
	}

	switch {
	case cfg.Verbose:
		level = ui.LevelDebug
	case cfg.Quiet:
		level = ui.LevelError
	}

	e := &env{
		cfg: cfg,
		ui: ui.New(ui.Options{
			Out: streams.Out, Err: streams.Err,
			Level: level, JSON: cfg.JSON, NoColor: cfg.NoColor || noColorFromEnv(), Quiet: cfg.Quiet,
		}),
		streams: streams,
		ctx:     ctx,
		cmd:     cmd,
	}

	if cfg.Timeout > 0 {
		var cancel context.CancelFunc

		e.ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	// --help is handled here rather than in each command: a per-command flag
	// would make every run closure refer to its own command variable, which Go
	// rejects as an initialisation cycle, and this reads better anyway.
	if wantsHelp(rest) {
		cmd.usage(streams.Out)

		return ExitOK, nil
	}

	if err := cmd.run(e, rest); err != nil {
		e.ui.Error(err)

		return ExitCode(err), nil
	}

	// A command that produced no reachable output did not succeed, whatever it
	// did to the database. A closed pipe or a full disk must not look like a
	// clean run to whatever is reading it.
	if err := e.ui.Failed(); err != nil {
		return ExitFailure, nil
	}

	return ExitOK, nil
}

// split separates the command name, its arguments and the global flags.
//
// Global flags are accepted before and after the command name, because both
// read naturally and forcing one order is a rule nobody remembers. The trap
// this avoids is binding the same struct twice: a subcommand FlagSet that also
// declares the globals resets them to their defaults on Parse, silently
// undoing anything given before the command name. Here the globals are
// collected into a map of what was actually typed, and only those keys are
// applied — so "typed" beats "defaulted" no matter which side they appeared on.
func split(args []string) (name string, rest []string, flags map[string]string, err error) {
	flags = map[string]string{}
	rest = []string{}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--":
			rest = append(rest, args[i+1:]...)

			return name, rest, flags, nil

		case arg == "-h" || arg == "--help" || arg == "help":
			if name == "" {
				name = "help"

				continue
			}

			rest = append(rest, "--help")

		case arg == "-v" || arg == "--version":
			if name == "" {
				name = "version"
			}

		case strings.HasPrefix(arg, "-"):
			key, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")

			spec, ok := globalSpec(key)
			if !ok {
				// Not a global: it belongs to the subcommand, which parses its
				// own flags and reports its own errors.
				rest = append(rest, arg)

				continue
			}

			if !hasValue {
				switch {
				case spec.Kind == config.KindBool:
					value = "true"
				case i+1 < len(args):
					i++
					value = args[i]
				default:
					return "", nil, nil, usageErrorf("--%s needs a value", spec.Flag)
				}
			}

			flags[spec.Flag] = value

		case name == "":
			name = arg

		default:
			rest = append(rest, arg)
		}
	}

	return name, rest, flags, nil
}

// globalSpec reports the global setting a flag name refers to.
func globalSpec(name string) (config.Spec, bool) {
	for _, s := range config.Specs {
		if !s.Global {
			continue
		}

		if s.Flag == name || (s.Short != "" && s.Short == name) {
			return s, true
		}
	}

	return config.Spec{}, false
}

// registry is every command, in the order --help lists them.
var registry []command

func lookup(name string) (command, bool) {
	for _, c := range registry {
		if c.name == name {
			return c, true
		}

		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}

	return command{}, false
}

// signalContext cancels ctx on the first SIGINT or SIGTERM and lets the second
// one kill the process.
//
// The first signal has to be a cancellation rather than an exit: a migration in
// flight holds a transaction and the advisory lock, and both need unwinding.
// The second is an escape hatch for the case where unwinding is itself stuck.
func signalContext(parent context.Context, errw io.Writer) (ctxOut context.Context, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 2)

	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case <-ch:
			fmt.Fprintln(errw, "migrator: interrupted, rolling back and releasing the lock")
			cancel()
		case <-ctx.Done():
			return
		}

		<-ch
		fmt.Fprintln(errw, "migrator: interrupted again, exiting now")
		os.Exit(ExitInterrupted)
	}()

	return ctx, func() { signal.Stop(ch); cancel() }
}

// noColorFromEnv honours the no-color.org convention.
func noColorFromEnv() bool {
	if os.Getenv("FORCE_COLOR") != "" {
		return false
	}

	_, set := os.LookupEnv("NO_COLOR")

	return set
}

// open builds a Migrator from the resolved configuration.
func (e *env) open(extra ...migrator.Option) (*migrator.Migrator, error) {
	if !e.cmd.needsDB {
		// A programming error, not a user one: a command that declared it needs
		// no database has just tried to open one.
		return nil, fmt.Errorf("migrator: command %q declared needsDB=false and opened a database", e.cmd.name)
	}

	dir := e.cfg.Dir

	opts := []migrator.Option{
		migrator.WithSchema(e.cfg.Schema),
		migrator.WithTable(e.cfg.Table),
		migrator.WithLogger(e.ui.Logger()),
		migrator.WithAdvisoryLockTimeout(e.cfg.AdvisoryLockTimeout),
		migrator.WithLockTimeout(e.cfg.LockTimeout),
		migrator.WithStatementTimeout(e.cfg.StatementTimeout),
		migrator.WithProgressInterval(e.cfg.ProgressInterval),
		migrator.WithEnvironment(e.environment()),
		migrator.WithMigratorTag(buildinfo.Version()),
	}

	if e.cfg.AllowOutOfOrder {
		opts = append(opts, migrator.WithOutOfOrder())
	}

	// Validate has already rejected an unparseable value, so a failure here can
	// only mean the two disagree, and silently running without the policy is
	// the one outcome worth avoiding.
	if e.cfg.MaxLockLevel != "" {
		level, ok := migrator.ParseLockLevel(e.cfg.MaxLockLevel)
		if !ok {
			return nil, fmt.Errorf("--max-lock-level: %q is not a lock level", e.cfg.MaxLockLevel)
		}

		opts = append(opts, migrator.WithMaxLockLevel(level))
	}

	opts = append(opts, extra...)

	m, err := migrator.New(migrator.FromDSN(e.cfg.DSN), os.DirFS(dir), opts...)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// environment decides what kind of database this is.
//
// The lean is towards production on purpose. Guessing wrong in that direction
// means a refusal, which costs one environment variable to correct and leaves
// the decision visible in shell history. Guessing wrong in the other direction
// means a dropped schema.
func (e *env) environment() migrator.Environment {
	if e.cfg.Env != "" {
		var env migrator.Environment
		if err := env.UnmarshalText([]byte(e.cfg.Env)); err == nil {
			return env
		}
	}

	for _, key := range []string{"MIGRATOR_ENV", "APP_ENV", "GO_ENV", "ENVIRONMENT"} {
		if v := os.Getenv(key); v != "" {
			var env migrator.Environment
			if err := env.UnmarshalText([]byte(v)); err == nil {
				return env
			}
		}
	}

	return inferEnvironment(e.cfg.DSN)
}

// splitList reads a comma-separated flag value.
func splitList(s string) []string {
	if s == "" {
		return nil
	}

	var out []string

	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// errorf is fmt.Errorf under another name, so that exit.go does not import fmt
// only for that.
func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// confirmed reports whether a destructive command may proceed.
//
// The absence of a terminal is a refusal, never a prompt nobody can answer and
// never a silent yes. Waiting for input that cannot arrive is how a tool wedges
// a CI job until its timeout.
// Naming a database is a stronger confirmation and is handled by --confirm,
// which the library checks against current_database(); this is only the y/N
// question for the operations where that would be overkill.
func (e *env) confirmed(prompt string, yes bool) error {
	if yes {
		return nil
	}

	if !isTerminal(e.streams.In) {
		return fmt.Errorf("%w: standard input is not a terminal; pass --yes",
			migrator.ErrNotConfirmed)
	}

	fmt.Fprint(e.streams.Err, prompt)

	var answer string
	if _, err := fmt.Fscanln(e.streams.In, &answer); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %w", migrator.ErrNotConfirmed, err)
	}

	switch strings.TrimSpace(answer) {
	case "y", "Y", "yes":
		return nil
	default:
		return fmt.Errorf("%w: not confirmed", migrator.ErrNotConfirmed)
	}
}

// isTerminal reports whether r is a terminal.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// flagSet builds a FlagSet that reports its errors through the usage machinery
// rather than printing to stderr on its own.
func flagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.Usage = func() {}

	return set
}

// wantsHelp reports whether the remaining arguments ask for the command's help.
func wantsHelp(args []string) bool {
	return slices.Contains(args, "--help") || slices.Contains(args, "-h")
}
