package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	migrator "github.com/efureev/db-migrator/v2"
	"github.com/efureev/db-migrator/v2/internal/buildinfo"
	"github.com/efureev/db-migrator/v2/internal/config"
)

func init() {
	registry = []command{
		upCmd, downCmd, redoCmd, statusCmd, validateCmd,
		createCmd, repairCmd, wipeCmd, configCmd, versionCmd, helpCmd,
	}
}

var upCmd = command{
	name: "up", summary: "apply every pending migration", needsDB: true,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator up [--to <version>] [--steps <n>] [--dry-run]

Applies pending migrations in ascending order. Each migration and the row
recording it share one transaction, so "applied but not recorded" cannot
happen. A migration carrying "-- migrator:no-transaction" runs outside one.

  --to <version>   stop after this version
  --steps <n>      apply at most n migrations
  --dry-run        print the plan and change nothing
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("up", e.streams.Err)
		to := fs.Int64("to", 0, "")
		steps := fs.Int("steps", 0, "")
		dryRun := fs.Bool("dry-run", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		target, err := targetOf(*to, *steps)
		if err != nil {
			return err
		}

		m, err := e.open()
		if err != nil {
			return err
		}

		if *dryRun {
			return e.showPlan(m, migrator.DirectionUp, target)
		}

		report, err := m.UpTo(e.ctx, target)
		if err != nil {
			return err
		}

		return e.ui.Render(report)
	},
}

var downCmd = command{
	name: "down", summary: "roll migrations back", needsDB: true,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator down --allow-down [--steps <n>] [--to <version>] [--dry-run] [--yes]

Rolls back the most recent migrations, newest first. Requires --allow-down,
and is refused outright when the environment is production — no flag overrides
that.

  --allow-down     required; says you meant it
  --steps <n>      roll back n migrations (default 1)
  --to <version>   roll back everything above this version, leaving it in force
  --dry-run        print the plan and change nothing
  --yes            do not ask for confirmation
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("down", e.streams.Err)
		to := fs.Int64("to", 0, "")
		steps := fs.Int("steps", 0, "")
		allow := fs.Bool("allow-down", false, "")
		dryRun := fs.Bool("dry-run", false, "")
		yes := fs.Bool("yes", false, "")
		fs.BoolVar(yes, "y", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		var target migrator.Target

		switch {
		case *to != 0 && *steps != 0:
			return usageErrorf("--to and --steps cannot both be given")
		case *to != 0:
			target = migrator.ToVersion(*to)
		case *steps != 0:
			target = migrator.Steps(*steps)
		default:
			// One step: rolling back everything by default is not a thing
			// anybody means by "down".
			target = migrator.Steps(1)
		}

		var opts []migrator.Option
		if *allow {
			opts = append(opts, migrator.WithAllowDown())
		}

		m, err := e.open(opts...)
		if err != nil {
			return err
		}

		if !*allow {
			return fmt.Errorf("%w\n\nRolling back changes the schema of a database something is "+
				"already running against.\nPass the flag once you have decided that is what you want:\n\n"+
				"  migrator down --steps 1 --allow-down", migrator.ErrDownNotAllowed)
		}

		if *dryRun {
			return e.showPlan(m, migrator.DirectionDown, target)
		}

		if err := e.confirmed("Roll back? [y/N]: ", "", *yes); err != nil {
			return err
		}

		report, err := m.Down(e.ctx, target)
		if err != nil {
			return err
		}

		return e.ui.Render(report)
	},
}

var redoCmd = command{
	name: "redo", summary: "roll back and re-apply the last migrations", needsDB: true,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator redo --allow-down [--steps <n>] [--yes]

Rolls back n migrations and applies them again, under one lock. This is the
development half of what "fresh" used to be; the destructive half is
"migrator wipe && migrator up", which is two commands on purpose.

  --allow-down   required; says you meant it
  --steps <n>    how many to redo (default 1)
  --yes          do not ask for confirmation
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("redo", e.streams.Err)
		steps := fs.Int("steps", 1, "")
		allow := fs.Bool("allow-down", false, "")
		yes := fs.Bool("yes", false, "")
		fs.BoolVar(yes, "y", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		if !*allow {
			return migrator.ErrDownNotAllowed
		}

		m, err := e.open(migrator.WithAllowDown())
		if err != nil {
			return err
		}

		if err := e.confirmed(fmt.Sprintf("Redo the last %d migration(s)? [y/N]: ", *steps), "", *yes); err != nil {
			return err
		}

		report, err := m.Redo(e.ctx, *steps)
		if err != nil {
			return err
		}

		return e.ui.Render(report)
	},
}

var statusCmd = command{
	name: "status", summary: "show what is applied and what is pending", needsDB: true,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator status [--check] [--current]

Reads the files and the journal and shows them side by side. It writes nothing:
no journal is created, no lock is taken, so it is safe against a replica and a
read-only role.

  --check     exit 3 when migrations are pending, 4 when files and journal
              disagree; for a CI step
  --current   print only the current version, for a script
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("status", e.streams.Err)
		check := fs.Bool("check", false, "")
		current := fs.Bool("current", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		m, err := e.open()
		if err != nil {
			return err
		}

		if *current {
			v, err := m.Version(e.ctx)
			if err != nil {
				return err
			}

			e.ui.Answer("%s\n", v)

			return nil
		}

		status, err := m.Status(e.ctx)
		if err != nil {
			return err
		}

		if err := e.ui.Render(status); err != nil {
			return err
		}

		if !*check {
			return nil
		}

		if drifted := status.Drifted(); len(drifted) > 0 {
			return fmt.Errorf("%w: %d migration(s) need attention",
				migrator.ErrChecksumMismatch, len(drifted))
		}

		if pending := status.Pending(); len(pending) > 0 {
			return &pendingError{n: len(pending)}
		}

		return nil
	},
}

// pendingError carries its own exit code, because "there is work to do" is
// neither a failure nor a success.
type pendingError struct{ n int }

func (e *pendingError) Error() string {
	return fmt.Sprintf("%d migration(s) pending", e.n)
}

var validateCmd = command{
	name: "validate", summary: "report every problem with the files and the journal", needsDB: true,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator validate [--strict] [--offline]

Reports every problem at once rather than stopping at the first: fixing a
drifted deployment one restart at a time is how a five-minute job becomes an
hour.

  --strict    treat a missing down file as an error, not a warning
  --offline   check only the files; no database is opened
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("validate", e.streams.Err)
		strict := fs.Bool("strict", false, "")
		offline := fs.Bool("offline", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		if *offline {
			set, err := migrator.Load(dirFS(e.cfg.Dir))
			if err != nil {
				return err
			}

			report := migrator.ValidateSource(set, *strict)

			if err := e.ui.Render(report); err != nil {
				return err
			}

			return report.Err()
		}

		m, err := e.open()
		if err != nil {
			return err
		}

		report, err := m.Validate(e.ctx)
		if err != nil {
			return err
		}

		if err := e.ui.Render(report); err != nil {
			return err
		}

		return report.Err()
	},
}

var createCmd = command{
	name: "create", aliases: []string{"new"},
	summary: "write a new pair of migration files",
	// No database. Writing a migration is something people do on a plane.
	needsDB: false,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator create <name> [--format timestamp|unix|sequential] [--no-down]

Writes <version>_<name>.up.sql and its down file. No database is opened.

  --format     timestamp (default), unix or sequential
  --no-down    write only the up file
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("create", e.streams.Err)
		format := fs.String("format", "timestamp", "")
		noDown := fs.Bool("no-down", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		name := strings.Join(fs.Args(), " ")
		if strings.TrimSpace(name) == "" {
			return usageErrorf("create needs a name: migrator create 'add users table'")
		}

		var opts []migrator.CreateOption

		switch *format {
		case "timestamp":
			opts = append(opts, migrator.WithVersionFormat(migrator.VersionTimestamp))
		case "unix":
			opts = append(opts, migrator.WithVersionFormat(migrator.VersionUnix))
		case "sequential":
			opts = append(opts, migrator.WithVersionFormat(migrator.VersionSequential))
		default:
			return usageErrorf("--format: %q is not timestamp, unix or sequential", *format)
		}

		if *noDown {
			opts = append(opts, migrator.WithoutDown())
		}

		pair, err := migrator.Create(e.cfg.Dir, name, opts...)
		if err != nil {
			return err
		}

		e.ui.Line(pair.UpPath)

		if pair.DownPath != "" {
			e.ui.Line(pair.DownPath)
		}

		return nil
	},
}

var repairCmd = command{
	name: "repair", summary: "fix the journal without touching the schema", needsDB: true,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator repair (--rehash <version> | --complete <version> |
                 --discard <version> | --prune) [--confirm <database>]

Edits the journal and never the schema. This exists so that no flag on "up"
has to: a --force on the command that applies migrations gets reached for in a
hurry, at exactly the moment the loud refusal was doing its job.

  --rehash <v>     record the file's current checksum for version v
  --complete <v>   mark an interrupted no-transaction migration as finished
  --discard <v>    forget version v, so that it runs again
  --prune          forget every recorded version whose file is gone
  --confirm <db>   name the database; required outside development
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("repair", e.streams.Err)
		rehash := fs.Int64("rehash", 0, "")
		complete := fs.Int64("complete", 0, "")
		discard := fs.Int64("discard", 0, "")
		prune := fs.Bool("prune", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		var ops []migrator.RepairOp

		if *rehash != 0 {
			ops = append(ops, migrator.RepairChecksum(*rehash))
		}

		if *complete != 0 {
			ops = append(ops, migrator.RepairComplete(*complete))
		}

		if *discard != 0 {
			ops = append(ops, migrator.RepairDiscard(*discard))
		}

		if *prune {
			ops = append(ops, migrator.RepairPrune())
		}

		if len(ops) == 0 {
			return usageErrorf("repair needs one of --rehash, --complete, --discard or --prune")
		}

		if len(ops) > 1 {
			return usageErrorf("repair takes one operation at a time, so that the command line " +
				"says exactly what it is about to edit")
		}

		m, err := e.open()
		if err != nil {
			return err
		}

		report, err := m.Repair(e.ctx, ops...)
		if err != nil {
			return err
		}

		for _, r := range report.Repairs {
			e.ui.Line("  " + r.String())
		}

		if len(report.Repairs) == 0 {
			e.ui.Line("  Nothing to repair.")
		}

		return nil
	},
}

var wipeCmd = command{
	name: "wipe", summary: "drop everything in the schema", needsDB: true,
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator wipe --allow-wipe --confirm <database> [--dry-run]

Drops every table, view, sequence, routine and type in the schema, in one
transaction, leaving the schema itself and its extensions alone. Refused
outright when the environment is production.

--yes is never enough here. --yes means "stop asking"; --confirm <database>
means "I know which database this is", and a typo in the name is what saves you.

  --allow-wipe       required; says you meant it
  --confirm <db>     the database name, checked against the connection
  --dry-run          list what would be dropped and drop nothing
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("wipe", e.streams.Err)
		allow := fs.Bool("allow-wipe", false, "")
		confirm := fs.String("confirm", "", "")
		dryRun := fs.Bool("dry-run", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		if !*allow {
			return fmt.Errorf("%w: wipe needs --allow-wipe", migrator.ErrWipeRefused)
		}

		var opts []migrator.Option
		if !*dryRun {
			opts = append(opts, migrator.WithAllowWipe())
		}

		m, err := e.open(opts...)
		if err != nil {
			return err
		}

		if *dryRun {
			e.ui.Note("dry run: nothing will be dropped")
		}

		if *confirm == "" {
			return fmt.Errorf("%w: pass --confirm <database>", migrator.ErrNotConfirmed)
		}

		report, err := m.Wipe(e.ctx, migrator.Confirm(*confirm))
		if err != nil {
			return err
		}

		for _, o := range report.Dropped {
			e.ui.Line("  dropped " + o.String())
		}

		for _, o := range report.Kept {
			e.ui.Line("  kept    " + o.String())
		}

		e.ui.Line("\n  " + report.String())

		return nil
	},
}

var configCmd = command{
	name: "config", summary: "show the resolved configuration and where it came from",
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator config [--show-secrets]

Prints every setting, its value and where the value came from. The password is
masked unless --show-secrets, and a connection string that does not parse is
not printed at all: the usual reason it fails to parse is a password holding a
character that needed escaping.
`)
	},
	run: func(e *env, args []string) error {
		fs := flagSet("config", e.streams.Err)
		showSecrets := fs.Bool("show-secrets", false, "")
		if err := fs.Parse(args); err != nil {
			return joinUsage(err)
		}

		e.ui.Answer("  %-24s %-40s %s\n", "SETTING", "VALUE", "FROM")

		for _, spec := range config.Specs {
			value := settingValue(e.cfg, spec)

			if spec.Secret && !*showSecrets {
				value = config.RedactedDSN(value)
			}

			e.ui.Answer("  %-24s %-40s %s\n", spec.Flag, value, e.cfg.Origin(spec.Flag))
		}

		e.ui.Answer("  %-24s %-40s %s\n", "environment", e.environment(), "inferred or set")

		return nil
	},
}

var versionCmd = command{
	name: "version", summary: "print the version of this binary",
	usage: func(w io.Writer) {
		fmt.Fprint(w, `migrator version

Prints the version of the binary. The version of the schema is a different
question, and "migrator status --current" answers it — version 1 used one word
for both, which was ambiguous every time somebody asked.
`)
	},
	run: func(e *env, _ []string) error {
		e.ui.Answer("%s\n", buildinfo.Long())

		return nil
	},
}

var helpCmd = command{
	name: "help", summary: "show this help",
	usage: func(w io.Writer) { usage(w) },
	run: func(e *env, args []string) error {
		if len(args) > 0 {
			if cmd, ok := lookup(args[0]); ok {
				cmd.usage(e.streams.Out)

				return nil
			}
		}

		usage(e.streams.Out)

		return nil
	},
}

// usage writes the top-level help.
func usage(w io.Writer) {
	fmt.Fprintf(w, `migrator %s — SQL migrations for PostgreSQL

Usage:
  migrator <command> [flags]

Commands:
`, buildinfo.Version())

	for _, c := range registry {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}

	fmt.Fprint(w, "\nGlobal flags:\n")

	for _, s := range config.Specs {
		if !s.Global {
			continue
		}

		name := "--" + s.Flag
		if s.Short != "" {
			name = "-" + s.Short + ", " + name
		}

		fmt.Fprintf(w, "  %-26s %s\n", name, s.Help)
	}

	fmt.Fprint(w, `
Exit codes:
  0  done, or nothing to do          4  files and journal disagree
  1  the run failed                  5  another migrator holds the lock
  2  the command was not understood  6  refused by a guard
  3  migrations are pending

Run "migrator help <command>" for the details of one command.
`)
}

// targetOf turns the --to and --steps flags into a Target.
func targetOf(to int64, steps int) (migrator.Target, error) {
	switch {
	case to != 0 && steps != 0:
		return migrator.Target{}, usageErrorf("--to and --steps cannot both be given")
	case to != 0:
		return migrator.ToVersion(to), nil
	case steps != 0:
		return migrator.Steps(steps), nil
	default:
		return migrator.All(), nil
	}
}

// showPlan renders what a run would do.
func (e *env) showPlan(m *migrator.Migrator, d migrator.Direction, t migrator.Target) error {
	plan, err := m.Plan(e.ctx, d, t)
	if err != nil {
		return err
	}

	if plan.Empty() {
		e.ui.Line("  Nothing to do.")

		return nil
	}

	e.ui.Line(fmt.Sprintf("  Plan: %d migration(s) %s", plan.Len(), d))

	for _, step := range plan.Steps {
		kind := "transactional"
		if !step.Transactional {
			kind = "no-transaction"
		}

		e.ui.Line(fmt.Sprintf("    %s  (%s, %d statement(s))", step.Migration, kind, len(step.SQL)))
	}

	return nil
}

// settingValue reports a setting's current value as text.
func settingValue(c *config.Config, spec config.Spec) string {
	switch spec.Flag {
	case "dsn":
		return c.DSN
	case "dir":
		return c.Dir
	case "schema":
		return c.Schema
	case "table":
		return c.Table
	case "env":
		return c.Env
	case "env-file":
		return c.EnvFile
	case "log-level":
		return c.LogLevel
	case "timeout":
		return c.Timeout.String()
	case "advisory-lock-timeout":
		return c.AdvisoryLockTimeout.String()
	case "lock-timeout":
		return c.LockTimeout.String()
	case "statement-timeout":
		return c.StatementTimeout.String()
	case "json":
		return strconv.FormatBool(c.JSON)
	case "no-color":
		return strconv.FormatBool(c.NoColor)
	case "quiet":
		return strconv.FormatBool(c.Quiet)
	case "verbose":
		return strconv.FormatBool(c.Verbose)
	case "allow-out-of-order":
		return strconv.FormatBool(c.AllowOutOfOrder)
	default:
		return ""
	}
}

// inferEnvironment guesses what kind of database a DSN points at.
//
// A non-loopback, non-private host or a production-looking database name reads
// as production. The bias is deliberate: see [env.environment].
func inferEnvironment(dsn string) migrator.Environment {
	if dsn == "" {
		// Nothing configured means PG* or a local socket, which is a
		// developer's machine far more often than anything else.
		return migrator.EnvDevelopment
	}

	host, database := hostAndDatabase(dsn)

	if database != "" {
		lower := strings.ToLower(database)
		for _, marker := range []string{"prod", "live"} {
			if strings.Contains(lower, marker) {
				return migrator.EnvProduction
			}
		}
	}

	if host == "" {
		return migrator.EnvDevelopment
	}

	if host == "localhost" || host == "host.docker.internal" {
		return migrator.EnvDevelopment
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// A name that is not obviously local: assume the worse of the two.
		return migrator.EnvProduction
	}

	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return migrator.EnvDevelopment
	}

	return migrator.EnvProduction
}

// hostAndDatabase pulls the host and database out of a DSN in either syntax.
func hostAndDatabase(dsn string) (host, database string) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", ""
		}

		return u.Hostname(), strings.TrimPrefix(u.Path, "/")
	}

	for _, field := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}

		switch key {
		case "host":
			host = value
		case "dbname":
			database = value
		}
	}

	return host, database
}
