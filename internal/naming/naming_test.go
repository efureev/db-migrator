package naming

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      string
		want    File
		wantErr error
	}{
		{
			name: "up file",
			in:   "20240117090000_create_users.up.sql",
			want: File{Version: 20240117090000, Name: "create_users", Half: Up},
		},
		{
			name: "down file",
			in:   "20240117090000_create_users.down.sql",
			want: File{Version: 20240117090000, Name: "create_users", Half: Down},
		},
		{
			name: "leading zeros are kept numerically",
			in:   "0001_init.up.sql",
			want: File{Version: 1, Name: "init", Half: Up},
		},
		{
			name: "unix timestamp from v1 still reads",
			in:   "1607057708_example_1.up.sql",
			want: File{Version: 1607057708, Name: "example_1", Half: Up},
		},
		{
			name: "digits in the name",
			in:   "5_v2_users.up.sql",
			want: File{Version: 5, Name: "v2_users", Half: Up},
		},

		{name: "not sql", in: "0001_init.up.txt", wantErr: ErrNotSQL},
		{name: "no half", in: "0001_init.sql", wantErr: ErrShape},
		{name: "no version", in: "init.up.sql", wantErr: ErrShape},
		{name: "no name", in: "0001_.up.sql", wantErr: ErrShape},
		{name: "upper case name", in: "0001_CreateUsers.up.sql", wantErr: ErrShape},
		{name: "upper case extension", in: "0001_init.UP.SQL", wantErr: ErrNotSQL},
		{name: "upper case half only", in: "0001_init.UP.sql", wantErr: ErrShape},
		{name: "doubled underscore", in: "0001_create__users.up.sql", wantErr: ErrShape},
		{name: "trailing underscore", in: "0001_create_.up.sql", wantErr: ErrShape},
		{name: "dash instead of underscore", in: "0001_create-users.up.sql", wantErr: ErrShape},
		{name: "unknown half", in: "0001_init.sideways.sql", wantErr: ErrShape},
		{name: "path, not a base name", in: "sub/0001_init.up.sql", wantErr: ErrShape},
		{name: "empty", in: "", wantErr: ErrNotSQL},
		{
			name:    "version overflows int64",
			in:      "9999999999999999999_init.up.sql",
			wantErr: ErrVersionRange,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(tc.in)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Parse(%q) error = %v, want %v", tc.in, err, tc.wantErr)
				}
				// The name must appear in the message: a directory of thirty
				// files is unfixable if the error does not say which one.
				if tc.in != "" && !strings.Contains(err.Error(), tc.in) {
					t.Errorf("error %q does not name the file %q", err, tc.in)
				}

				return
			}

			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tc.in, err)
			}

			tc.want.Raw = tc.in
			if got != tc.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseRejectsNumericallyEqualSpellings pins the decision that 0001 and 1
// are one version. Load reports them as a duplicate rather than picking one,
// and this is the property that makes that possible.
func TestParseRejectsNumericallyEqualSpellings(t *testing.T) {
	t.Parallel()

	a, err := Parse("0001_init.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	b, err := Parse("1_init.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if a.Version != b.Version {
		t.Fatalf("0001 and 1 parsed to %d and %d; they must collide", a.Version, b.Version)
	}
}

func TestStem(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"0003_notify.up.sql",
		"0003_notify.down.sql",
		"20240117090000_create_users.up.sql",
	} {
		f, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}

		want, _, _ := strings.Cut(in, ".")
		if got := f.Stem(); got != want {
			t.Errorf("Parse(%q).Stem() = %q, want %q", in, got, want)
		}
	}
}

func TestFormatRoundTrips(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version int64
		name    string
		half    Half
		want    string
	}{
		{1, "init", Up, "1_init.up.sql"},
		{20240117090000, "create_users", Down, "20240117090000_create_users.down.sql"},
	} {
		got := Format(tc.version, tc.name, tc.half)
		if got != tc.want {
			t.Fatalf("Format(%d, %q, %v) = %q, want %q", tc.version, tc.name, tc.half, got, tc.want)
		}

		back, err := Parse(got)
		if err != nil {
			t.Fatalf("Parse(Format(...)) = %v", err)
		}

		if back.Version != tc.version || back.Name != tc.name || back.Half != tc.half {
			t.Errorf("round trip lost data: %+v", back)
		}
	}
}

func TestSnake(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"create_users":       "create_users",
		"Create Users Table": "create_users_table",
		"createUsersTable":   "create_users_table",
		"CreateUsersTable":   "create_users_table",
		"HTTPServer":         "http_server",
		"parseHTTPResponse":  "parse_http_response",
		"add-email-index":    "add_email_index",
		"  spaced   out  ":   "spaced_out",
		"v2Table":            "v2_table",
		"already_snake_1":    "already_snake_1",
		"__leading":          "leading",
		"trailing__":         "trailing",
		"a":                  "a",
		"":                   "",
		"!!!":                "",
		"добавить таблицу":   "", // не-ASCII отбрасывается: формат имени — ASCII
	}

	for in, want := range cases {
		if got := Snake(in); got != want {
			t.Errorf("Snake(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSnakeProducesParsableNames is the property that matters: whatever a human
// types, Create must be able to build a file name Parse accepts.
func TestSnakeProducesParsableNames(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"Create Users Table", "createUsersTable", "add-email-index",
		"  spaced   out  ", "v2Table", "HTTPServer", "a", "9",
	} {
		got := Snake(in)
		if got == "" {
			t.Fatalf("Snake(%q) produced an empty name", in)
		}

		if !Valid(got) {
			t.Errorf("Snake(%q) = %q, which Valid rejects", in, got)
		}

		if _, err := Parse(Format(1, got, Up)); err != nil {
			t.Errorf("Snake(%q) = %q, which Parse rejects: %v", in, got, err)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"0001_init.up.sql", "1_a.down.sql", "20240117090000_create_users.up.sql",
		"", ".sql", "_.up.sql", "0001_init.up.sql.sql",
		"99999999999999999999_x.up.sql", "0001_init.UP.sql",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got, err := Parse(name)
		if err != nil {
			return
		}

		// Anything Parse accepts must round-trip through its own parts, and the
		// accepted name must be exactly what Format would have produced apart
		// from leading zeros in the version.
		if got.Raw != name {
			t.Fatalf("Parse(%q).Raw = %q", name, got.Raw)
		}

		if !Valid(got.Name) {
			t.Fatalf("Parse(%q) accepted an invalid name %q", name, got.Name)
		}

		if got.Version < 0 {
			t.Fatalf("Parse(%q) produced a negative version %d", name, got.Version)
		}

		again, err := Parse(Format(got.Version, got.Name, got.Half))
		if err != nil {
			t.Fatalf("Parse(%q) accepted, but its own Format output does not: %v", name, err)
		}

		if again.Version != got.Version || again.Name != got.Name || again.Half != got.Half {
			t.Fatalf("round trip changed %q into %+v", name, again)
		}

		if !strings.HasPrefix(name, got.Stem()) {
			t.Fatalf("Parse(%q).Stem() = %q is not a prefix of the name", name, got.Stem())
		}
	})
}

func FuzzSnake(f *testing.F) {
	for _, seed := range []string{
		"Create Users", "createUsers", "HTTPServer", "", "___", "9", "a-b_c d",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got := Snake(in)

		// The only postcondition worth asserting is the one Create depends on:
		// the result is either empty, or a name Parse will accept.
		if got == "" {
			return
		}

		if !Valid(got) {
			t.Fatalf("Snake(%q) = %q, which Valid rejects", in, got)
		}

		if Snake(got) != got {
			t.Fatalf("Snake is not idempotent: Snake(%q) = %q, Snake(%q) = %q",
				in, got, got, Snake(got))
		}
	})
}
