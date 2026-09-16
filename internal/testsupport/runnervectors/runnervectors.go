// Package runnervectors holds the app.CommandRunner capture contract as
// shared vectors: the per-stream output bounds, truncation flags and bound
// refusal the real process runner (internal/adapters/process) implements,
// expressed once so the runner's own tests and every handwritten
// CommandRunner fake (internal/app, internal/adapters/sqlite) run the
// IDENTICAL cases against their own implementation. It also holds
// BoundCapture, the reference implementation of that contract the fakes
// apply to their answers, so a fake cannot drift from the runner by
// omitting it.
package runnervectors

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
)

// DefaultCaptureBytes is the per-stream capture bound of a command that
// sets no MaxOutputBytes: the real runner's default.
const DefaultCaptureBytes = 1 << 20

// Fill is the byte every vector's output consists of.
const Fill = 'y'

// ErrNegativeBound is a fake's refusal of a negative MaxOutputBytes; the
// real runner refuses the same command, with its own error, before
// anything starts.
var ErrNegativeBound = errors.New("runnervectors: the output bound is negative")

// CaptureVector is one capture case: a command that writes StdoutBytes and
// StderrBytes of Fill and exits with ExitCode, run with MaxOutputBytes, and
// what every CommandRunner must report for it.
type CaptureVector struct {
	Name                     string
	StdoutBytes, StderrBytes int
	ExitCode                 int
	MaxOutputBytes           int
	// Refused means the runner refuses the command before anything starts.
	Refused bool
	// KeptStdout and KeptStderr are the captured lengths.
	KeptStdout, KeptStderr           int
	StdoutTruncated, StderrTruncated bool
}

// CaptureVectors returns the capture contract's cases: the default bound
// and a per-call bound, each applied to every stream separately; a stream
// exactly at its bound complete and one byte more truncated, whatever the
// exit status; a per-call bound above and below the default; and a
// negative bound refused.
func CaptureVectors() []CaptureVector {
	const mib = DefaultCaptureBytes
	return []CaptureVector{
		{Name: "small output", StdoutBytes: 10, StderrBytes: 5, KeptStdout: 10, KeptStderr: 5},
		{Name: "empty output"},
		{Name: "exactly the default bound is complete", StdoutBytes: mib, KeptStdout: mib},
		{Name: "one byte past the default bound", StdoutBytes: mib + 1, KeptStdout: mib, StdoutTruncated: true},
		{Name: "truncated on a successful exit", StdoutBytes: 3 * mib, KeptStdout: mib, StdoutTruncated: true},
		{Name: "truncated on a failing exit", StdoutBytes: 3 * mib, ExitCode: 3, KeptStdout: mib, StdoutTruncated: true},
		{Name: "stderr alone", StdoutBytes: 7, StderrBytes: mib + 1, KeptStdout: 7, KeptStderr: mib, StderrTruncated: true},
		{Name: "both streams", StdoutBytes: mib + 9, StderrBytes: 2 * mib, KeptStdout: mib, KeptStderr: mib, StdoutTruncated: true, StderrTruncated: true},
		{Name: "a larger per-call bound keeps output past the default", StdoutBytes: 2*mib + 1, MaxOutputBytes: 3 * mib, KeptStdout: 2*mib + 1},
		{Name: "exactly a per-call bound is complete", StdoutBytes: 2 * mib, MaxOutputBytes: 2 * mib, KeptStdout: 2 * mib},
		{Name: "one byte past a per-call bound", StdoutBytes: 2*mib + 1, MaxOutputBytes: 2 * mib, KeptStdout: 2 * mib, StdoutTruncated: true},
		{Name: "a per-call bound applies to each stream", StdoutBytes: 3 * mib, StderrBytes: 2 * mib, MaxOutputBytes: 2 * mib, KeptStdout: 2 * mib, KeptStderr: 2 * mib, StdoutTruncated: true},
		{Name: "a per-call bound below the default", StdoutBytes: 101, StderrBytes: 100, MaxOutputBytes: 100, KeptStdout: 100, KeptStderr: 100, StdoutTruncated: true},
		{Name: "a one-byte per-call bound", StdoutBytes: 19, MaxOutputBytes: 1, KeptStdout: 1, StdoutTruncated: true},
		{Name: "a negative bound is refused", StdoutBytes: 10, MaxOutputBytes: -1, Refused: true},
	}
}

// Answer is the complete, unbounded result a fake answers the vector's
// command with, before BoundCapture.
func (v *CaptureVector) Answer() app.CommandResult {
	return app.CommandResult{
		ExitCode: v.ExitCode,
		Stdout:   bytes.Repeat([]byte{Fill}, v.StdoutBytes),
		Stderr:   bytes.Repeat([]byte{Fill}, v.StderrBytes),
	}
}

// Check reports how a runner's result for the vector's command departs
// from the contract, or nil when it matches: a refused vector must return
// an error, and any other must return no error, the exit code, exactly the
// kept prefix of each stream, and exactly the expected flags.
func (v *CaptureVector) Check(result app.CommandResult, err error) error {
	if v.Refused {
		if err == nil {
			return fmt.Errorf("%s: the command ran; a negative bound is refused before anything starts", v.Name)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", v.Name, err)
	}
	var problems []error
	if result.ExitCode != v.ExitCode {
		problems = append(problems, fmt.Errorf("exit code %d, want %d", result.ExitCode, v.ExitCode))
	}
	for _, stream := range []struct {
		name          string
		kept          []byte
		truncated     bool
		wantKept      int
		wantTruncated bool
	}{
		{"stdout", result.Stdout, result.StdoutTruncated, v.KeptStdout, v.StdoutTruncated},
		{"stderr", result.Stderr, result.StderrTruncated, v.KeptStderr, v.StderrTruncated},
	} {
		if len(stream.kept) != stream.wantKept || bytes.Count(stream.kept, []byte{Fill}) != len(stream.kept) {
			problems = append(problems, fmt.Errorf("%s kept %d bytes, want the first %d", stream.name, len(stream.kept), stream.wantKept))
		}
		if stream.truncated != stream.wantTruncated {
			problems = append(problems, fmt.Errorf("%s truncated = %v, want %v", stream.name, stream.truncated, stream.wantTruncated))
		}
	}
	if len(problems) != 0 {
		return fmt.Errorf("%s: %w", v.Name, errors.Join(problems...))
	}
	return nil
}

// ValidateBound is the runner's refusal of a negative bound, for a fake
// to apply before it answers anything.
func ValidateBound(maxOutputBytes int) error {
	if maxOutputBytes < 0 {
		return ErrNegativeBound
	}
	return nil
}

// BoundCapture applies the capture contract to a fake's complete answer:
// each stream keeps its first maxOutputBytes bytes (DefaultCaptureBytes
// when 0) and is flagged truncated exactly when bytes were discarded,
// whatever the exit status. A negative bound is refused. An answer that
// already claims truncation must hold exactly the bound — the only
// truncated shape the real runner reports — and any other claim is
// refused as a fake misuse.
func BoundCapture(result app.CommandResult, maxOutputBytes int) (app.CommandResult, error) {
	if err := ValidateBound(maxOutputBytes); err != nil {
		return app.CommandResult{}, err
	}
	limit := DefaultCaptureBytes
	if maxOutputBytes > 0 {
		limit = maxOutputBytes
	}
	var err error
	if result.Stdout, result.StdoutTruncated, err = boundStream(result.Stdout, result.StdoutTruncated, limit); err != nil {
		return app.CommandResult{}, fmt.Errorf("runnervectors: stdout: %w", err)
	}
	if result.Stderr, result.StderrTruncated, err = boundStream(result.Stderr, result.StderrTruncated, limit); err != nil {
		return app.CommandResult{}, fmt.Errorf("runnervectors: stderr: %w", err)
	}
	return result, nil
}

func boundStream(stream []byte, claimed bool, limit int) (kept []byte, truncated bool, err error) {
	switch {
	case len(stream) > limit:
		return stream[:limit:limit], true, nil
	case claimed && len(stream) != limit:
		return nil, false, fmt.Errorf("an answer claims truncation with %d captured bytes; the real runner reports truncation only with exactly %d", len(stream), limit)
	default:
		return stream, claimed, nil
	}
}
