package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	if !report.Healthy() {
		return exitFailure, nil
	}
	return exitOK, nil
}

// renderReport prints one aligned line per check, an advice line under every
// check that carries one, and a closing summary.
func renderReport(w io.Writer, report app.Report) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(w, "%-12s %s: %s\n", check.Status, check.Name, check.Detail); err != nil {
			return err
		}
		if check.Advice != "" {
			if _, err := fmt.Fprintf(w, "%-12s   advice: %s\n", "", check.Advice); err != nil {
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
