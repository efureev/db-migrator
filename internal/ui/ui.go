// Package ui is the only place in this program that writes to a terminal.
//
// Two streams, and the split is not cosmetic. Standard output carries the
// answer to the question that was asked — the status table, the JSON, the paths
// a create wrote — and nothing else, so that a caller can pipe it. Standard
// error carries the running commentary and the failures, so that a pipeline
// reading stdout is not corrupted by a progress line.
//
// Everything that renders lives here for one more reason: --json has to be
// obeyed by every line the program emits, and the only way to be sure of that
// is for there to be one place that emits. A stray fmt.Println elsewhere is a
// linter error (forbidigo), not a review comment.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/efureev/reggol"
	"github.com/efureev/reggol/slogr"
)

// A Level is how much running commentary to write.
type Level int

// Levels, in the order a person thinks about them.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel reads a level name.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info", "":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q: want debug, info, warn or error", s)
	}
}

// String reports the level as it is spelled in configuration.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	case LevelInfo:
		return "info"
	default:
		return "info"
	}
}

// reggolLevel maps onto the logger's own scale.
func (l Level) reggolLevel() reggol.Level {
	switch l {
	case LevelDebug:
		return reggol.DebugLevel
	case LevelWarn:
		return reggol.WarnLevel
	case LevelError:
		return reggol.ErrorLevel
	case LevelInfo:
		return reggol.InfoLevel
	default:
		return reggol.InfoLevel
	}
}

// Options configure a [UI].
type Options struct {
	// Out and Err are the two streams. Both must be set.
	Out io.Writer
	Err io.Writer
	// Level is how much commentary to write to Err.
	Level Level
	// JSON switches every renderer to machine-readable output.
	JSON bool
	// NoColor forces plain text even when Err looks like a terminal.
	NoColor bool
	// Quiet suppresses everything on Out except the answer itself.
	Quiet bool
}

// A UI renders everything this program shows a person.
type UI struct {
	out, errw *recordingWriter
	log       *slog.Logger
	opts      Options
}

// recordingWriter remembers the first write error instead of discarding it.
//
// Individual Fprintf calls do not check: doing so at every call site is noise,
// and there is nowhere useful to report a failed write to stderr anyway. But a
// closed pipe or a full disk must not look like success, so the first failure
// is kept and [UI.Failed] reports it once, at the end, where the exit code is
// decided.
type recordingWriter struct {
	w   io.Writer
	err error
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	n, err := r.w.Write(p)
	if err != nil && r.err == nil {
		r.err = err
	}

	return n, err
}

// Failed reports the first write error, if the output never arrived.
func (u *UI) Failed() error {
	if u.out.err != nil {
		return fmt.Errorf("writing the answer: %w", u.out.err)
	}

	return nil
}

// New builds a UI.
//
// reggol is constructed here and nowhere else in the program; the rest of the
// code sees a *slog.Logger. That keeps the choice of logger an implementation
// detail of the binary rather than something the library or the command layer
// depends on — which is the same reason the library takes a *slog.Logger.
func New(opts Options) *UI {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}

	if opts.Err == nil {
		opts.Err = os.Stderr
	}

	mode := reggol.ColorAuto
	if opts.NoColor {
		mode = reggol.ColorNever
	}

	var enc reggol.Encoder
	if opts.JSON {
		// With --json the commentary has to be machine-readable too, or a
		// consumer parsing stderr breaks on the first progress line.
		enc = reggol.NewJSONEncoder()
	} else {
		enc = reggol.NewConsoleEncoder(reggol.WithColorMode(mode, opts.Err))
	}

	logger := reggol.New(opts.Err,
		reggol.WithEncoder(enc),
		reggol.WithLevel(opts.Level.reggolLevel()),
	)

	return &UI{
		out:  &recordingWriter{w: opts.Out},
		errw: &recordingWriter{w: opts.Err},
		log:  slogr.New(logger),
		opts: opts,
	}
}

// Logger reports the logger to hand the library.
func (u *UI) Logger() *slog.Logger { return u.log }

// JSON reports whether machine-readable output was asked for.
func (u *UI) JSON() bool { return u.opts.JSON }

// Out reports the answer stream, for a renderer that writes to it directly.
func (u *UI) Out() io.Writer { return u.out }

// Err reports the commentary stream.
func (u *UI) Err() io.Writer { return u.errw }

// Answer writes to standard output. This is for the thing that was asked for.
func (u *UI) Answer(format string, args ...any) {
	fmt.Fprintf(u.out, format, args...)
}

// Line writes one line to standard output, unless --quiet.
func (u *UI) Line(s string) {
	if u.opts.Quiet {
		return
	}

	fmt.Fprintln(u.out, s)
}

// Note writes a line of commentary to standard error, unless --quiet.
func (u *UI) Note(format string, args ...any) {
	if u.opts.Quiet || u.opts.JSON {
		return
	}

	fmt.Fprintf(u.errw, format+"\n", args...)
}

// Error writes a failure to standard error.
//
// Failures survive --quiet: silencing progress is a request for less noise, not
// for a program that fails without saying so.
func (u *UI) Error(err error) {
	if u.opts.JSON {
		_ = json.NewEncoder(u.errw).Encode(struct {
			Error string `json:"error"`
		}{err.Error()})

		return
	}

	fmt.Fprintln(u.errw, "migrator: "+err.Error())
}

// Renderable is anything this program can show, in either shape.
type Renderable interface {
	// Text writes the value for a person.
	Text(w io.Writer) error
	// JSON writes the value for a machine.
	JSON(w io.Writer) error
}

// Render writes a value in whichever shape was asked for.
func (u *UI) Render(r Renderable) error {
	if u.opts.JSON {
		return r.JSON(u.out)
	}

	return r.Text(u.out)
}
