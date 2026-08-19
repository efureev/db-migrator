package ui

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// failingWriter refuses every write, standing in for a closed pipe.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestParseLevel(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]Level{
		"debug": LevelDebug, "info": LevelInfo, "": LevelInfo,
		"warn": LevelWarn, "warning": LevelWarn, "error": LevelError,
		"  DEBUG  ": LevelDebug,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	if _, err := ParseLevel("loud"); err == nil {
		t.Error("ParseLevel accepted an unknown level")
	}

	for level, want := range map[Level]string{
		LevelDebug: "debug", LevelInfo: "info", LevelWarn: "warn", LevelError: "error",
		Level(99): "info",
	} {
		if got := level.String(); got != want {
			t.Errorf("Level(%d) = %q, want %q", level, got, want)
		}
	}
}

// TestStreamsAreSeparate is the property a pipeline depends on: the answer is
// on stdout and the commentary is not.
func TestStreamsAreSeparate(t *testing.T) {
	t.Parallel()

	var out, errw bytes.Buffer

	u := New(Options{Out: &out, Err: &errw, NoColor: true})

	u.Answer("answer %d\n", 1)
	u.Line("line")
	u.Note("note %d", 2)
	u.Error(errors.New("boom"))

	if !strings.Contains(out.String(), "answer 1") || !strings.Contains(out.String(), "line") {
		t.Errorf("stdout = %q", out.String())
	}

	if strings.Contains(out.String(), "note") || strings.Contains(out.String(), "boom") {
		t.Errorf("commentary leaked onto stdout: %q", out.String())
	}

	if !strings.Contains(errw.String(), "note 2") || !strings.Contains(errw.String(), "boom") {
		t.Errorf("stderr = %q", errw.String())
	}
}

// TestQuietKeepsFailures: silencing progress is a request for less noise, not
// for a program that fails without saying so.
func TestQuietKeepsFailures(t *testing.T) {
	t.Parallel()

	var out, errw bytes.Buffer

	u := New(Options{Out: &out, Err: &errw, Quiet: true, NoColor: true})

	u.Line("progress")
	u.Note("progress")
	u.Error(errors.New("boom"))

	if out.Len() != 0 {
		t.Errorf("--quiet still wrote to stdout: %q", out.String())
	}

	if !strings.Contains(errw.String(), "boom") {
		t.Errorf("--quiet swallowed the failure: %q", errw.String())
	}
}

// TestJSONErrorIsMachineReadable: with --json every line the program emits has
// to be parseable, including the failure.
func TestJSONErrorIsMachineReadable(t *testing.T) {
	t.Parallel()

	var out, errw bytes.Buffer

	u := New(Options{Out: &out, Err: &errw, JSON: true})

	u.Error(errors.New("boom"))

	if !strings.HasPrefix(strings.TrimSpace(errw.String()), `{"error"`) {
		t.Errorf("stderr = %q, want JSON", errw.String())
	}

	if !u.JSON() {
		t.Error("JSON() reports false after being asked for JSON")
	}
}

type renderable struct{ text, json string }

func (r renderable) Text(w io.Writer) error { _, err := io.WriteString(w, r.text); return err }
func (r renderable) JSON(w io.Writer) error { _, err := io.WriteString(w, r.json); return err }

func TestRenderPicksTheShape(t *testing.T) {
	t.Parallel()

	value := renderable{text: "as text", json: `{"as":"json"}`}

	var plain bytes.Buffer
	if err := New(Options{Out: &plain, Err: io.Discard, NoColor: true}).Render(value); err != nil {
		t.Fatal(err)
	}

	if plain.String() != "as text" {
		t.Errorf("text mode wrote %q", plain.String())
	}

	var machine bytes.Buffer
	if err := New(Options{Out: &machine, Err: io.Discard, JSON: true}).Render(value); err != nil {
		t.Fatal(err)
	}

	if machine.String() != `{"as":"json"}` {
		t.Errorf("json mode wrote %q", machine.String())
	}
}

// TestFailedReportsALostAnswer: a command whose output never arrived did not
// succeed, whatever it did to the database.
func TestFailedReportsALostAnswer(t *testing.T) {
	t.Parallel()

	u := New(Options{Out: failingWriter{}, Err: io.Discard, NoColor: true})

	if err := u.Failed(); err != nil {
		t.Errorf("Failed reported a problem before anything was written: %v", err)
	}

	u.Answer("anything")

	if err := u.Failed(); err == nil {
		t.Error("a write to a broken pipe was not recorded")
	}

	// A working stream stays clean.
	ok := New(Options{Out: io.Discard, Err: io.Discard, NoColor: true})
	ok.Answer("anything")

	if err := ok.Failed(); err != nil {
		t.Errorf("Failed reported a problem on a healthy stream: %v", err)
	}
}

func TestLoggerIsUsable(t *testing.T) {
	t.Parallel()

	var errw bytes.Buffer

	u := New(Options{Out: io.Discard, Err: &errw, Level: LevelDebug, NoColor: true})

	u.Logger().Info("hello", "key", "value")

	if !strings.Contains(errw.String(), "hello") {
		t.Errorf("the logger wrote %q", errw.String())
	}

	// Logging goes to stderr, never to the answer stream.
	if u.Out() == nil || u.Err() == nil {
		t.Error("the streams are not exposed")
	}
}

// TestPrefixedNamesTheProgramOnce: library errors carry "migrator: " already,
// because that is what an error from a Go package looks like in godoc and in a
// caller's own log. The terminal wants the name once.
func TestPrefixedNamesTheProgramOnce(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"migrator: the journal table belongs to another tool": "migrator: the journal table belongs to another tool",
		`unknown command "nope"`:                              `migrator: unknown command "nope"`,
		"":                                                    "migrator: ",
		"migratory birds":                                     "migrator: migratory birds",
	}

	for in, want := range cases {
		if got := Prefixed(in); got != want {
			t.Errorf("Prefixed(%q) = %q, want %q", in, got, want)
		}
	}
}
