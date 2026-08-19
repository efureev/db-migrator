// Package buildinfo reports the version of this binary.
//
// The values are stamped by the linker at release time and fall back to what
// the Go toolchain records when they are not — so `go install` produces a
// binary that still knows what it is, rather than one that says "unknown".
//
// Version 1 stamped a package path that did not exist: -X was pointed at
// "migrator/src/commands.version" while the module was
// "github.com/efureev/db-migrator". The linker ignores an unknown symbol
// silently, so every release for two years printed "unknown (unknown)" and
// nothing complained. ldflags_test.go now reads build.sh and Dockerfile and
// checks the path in them against this package's real one.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Stamped by the linker; see build.sh.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Version reports the release version, or the module version Go recorded, or
// "dev".
func Version() string {
	if version != "" {
		return version
	}

	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return "dev"
}

// Commit reports the commit this was built from.
func Commit() string {
	if commit != "" {
		return commit
	}

	return vcs("vcs.revision")
}

// Date reports when this was built.
func Date() string {
	if date != "" {
		return date
	}

	return vcs("vcs.time")
}

// Long reports everything on one line, which is what `migrator version` prints.
func Long() string {
	var b strings.Builder

	b.WriteString("migrator ")
	b.WriteString(Version())

	if c := Commit(); c != "" {
		b.WriteString(" (")
		b.WriteString(short(c))
		b.WriteString(")")
	}

	if d := Date(); d != "" {
		b.WriteString(", built ")
		b.WriteString(d)
	}

	return b.String()
}

// vcs reads one of the build settings the toolchain records for a repository
// checkout.
func vcs(key string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}

	return ""
}

func short(s string) string {
	const n = 12
	if len(s) <= n {
		return s
	}

	return s[:n]
}
