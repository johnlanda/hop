package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// defaultDoctorTimeout bounds the whole doctor run: every probe subprocess
// and the socket ping share this deadline.
const defaultDoctorTimeout = 10 * time.Second

// runDoctor implements `hop doctor`: it probes the herdr installation, the
// configured server socket and the supported native harnesses, prints one
// line per check and exits 1 when a required capability is unsupported or
// unavailable. getenv supplies the defaults for the flag values.
func runDoctor(args []string, stdout, stderr io.Writer, getenv func(string) string) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop doctor", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	herdrPath := flags.String("herdr", "", "herdr binary to probe (default $HERDR_BIN_PATH, then PATH)")
	socketPath := flags.String("socket", "", "herdr server socket to ping (default $HERDR_SOCKET_PATH)")
	timeout := flags.Duration("timeout", defaultDoctorTimeout, "deadline for the whole doctor run")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop doctor: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	if *herdrPath == "" {
		*herdrPath = getenv("HERDR_BIN_PATH")
	}
	if *socketPath == "" {
		*socketPath = getenv("HERDR_SOCKET_PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	doctor := &app.Doctor{
		Probe:      &herdr.InstallationProbe{BinaryPath: *herdrPath, SocketPath: *socketPath},
		SocketPath: *socketPath,
	}
	report := doctor.Run(ctx)
	if err := renderReport(stdout, report); err != nil {
		return exitOK, err
	}
	rootHealthy, err := renderStateRoot(stdout, getenv)
	if err != nil {
		return exitOK, err
	}
	if !report.Healthy() || !rootHealthy {
		return exitFailure, nil
	}
	return exitOK, nil
}

// renderStateRoot prints the doctor's store-path line: the state root the
// shared resolver computes — without opening or migrating the database —
// with its source label (override or default) and whether the store file
// already exists (docs/plan/phase-2-design.md section 4). healthy is false
// when the resolver refuses the environment, such as a relative
// HOP_STATE_DIR. The returned error is non-nil only for a failed write.
func renderStateRoot(w io.Writer, getenv func(string) string) (healthy bool, err error) {
	root, source, resolveErr := resolveStateRoot(getenv)
	if resolveErr != nil {
		_, err = fmt.Fprintf(w, "%-12s state root: %s\n", "unavailable", resolveErr)
		return false, err
	}
	status := "store absent (created on first run)"
	if _, statErr := os.Stat(filepath.Join(root, "hop.db")); statErr == nil {
		status = "store present"
	}
	_, err = fmt.Fprintf(w, "%-12s state root: %s (%s; %s)\n", "ok", root, source, status)
	return true, err
}

// renderReport prints one aligned line per check, an advice line under every
// check that carries one, and a closing summary.
func renderReport(w io.Writer, report app.Report) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(w, "%-12s %s: %s\n", check.Status, check.Name, safeRenderExternal(check.Detail)); err != nil {
			return err
		}
		if check.Advice != "" {
			if _, err := fmt.Fprintf(w, "%-12s   advice: %s\n", "", safeRenderExternal(check.Advice)); err != nil {
				return err
			}
		}
	}
	if problems := report.Problems(); problems > 0 {
		_, err := fmt.Fprintf(w, "hop doctor: %d problem(s) found\n", problems)
		return err
	}
	_, err := fmt.Fprint(w, "hop doctor: no problems found\n")
	return err
}
