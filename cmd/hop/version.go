package main

import (
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// version is the release version stamped at build time through
// -ldflags "-X main.version=<value>". It stays empty for builds that do not
// stamp it; resolveVersion then falls back to the module build information.
var version string

// devVersion is reported when neither a stamped version nor module build
// information identifies the build.
const devVersion = "devel"

// runVersion implements `hop version`: it accepts no flags or arguments and
// prints the resolved version with the Go toolchain and platform of the build.
// The error is non-nil only when writing to stdout or stderr failed.
func runVersion(args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("hop version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return exitUsage, nil
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop version: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		info = nil
	}
	_, err := fmt.Fprintf(stdout, "hop version %s (%s %s/%s)\n",
		resolveVersion(version, info), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return exitOK, err
}

// resolveVersion picks the version to report. A stamped version wins. Otherwise
// the main module version from build information is used when the toolchain
// recorded one, then a version-control revision when only that is known, and
// finally devVersion. A nil info means no build information is available.
func resolveVersion(stamped string, info *debug.BuildInfo) string {
	if stamped != "" {
		return stamped
	}
	if info == nil {
		return devVersion
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return devVersion
	}
	var b strings.Builder
	b.WriteString(devVersion)
	b.WriteString("+")
	b.WriteString(shortRevision(revision))
	if modified {
		b.WriteString(".modified")
	}
	return b.String()
}

// shortRevisionLength is the number of leading revision characters reported.
const shortRevisionLength = 12

// shortRevision abbreviates a version-control revision for display.
func shortRevision(revision string) string {
	if len(revision) <= shortRevisionLength {
		return revision
	}
	return revision[:shortRevisionLength]
}
