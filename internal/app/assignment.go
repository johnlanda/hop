package app

import "fmt"

// assignmentFields are the values rendered into the assignment artifact.
// Delivery is by reference in the launch argv, never typed into a running
// dialog: the file is the brief and the full worker instructions as one
// immutable artifact (docs/plan/phase-2-design.md section 6).
type assignmentFields struct {
	RunID, TaskID, AttemptID string
	Brief                    string
	AssignmentPath           string // absolute
	HOPPath                  string // absolute
}

// renderAssignment renders the assignment artifact's content: a fixed
// template containing only absolute paths, identities and the brief text —
// no shell quoting concerns, since the file is read by the worker rather
// than typed into a command line. Rendering is pure and deterministic, so
// the same fields always produce the same bytes and therefore the same
// digest.
func renderAssignment(f *assignmentFields) []byte {
	return fmt.Appendf(nil, `# HOP Assignment

Run: %s
Task: %s
Attempt: %s

## Brief

%s

## Instructions

Read this file at its absolute path (%s) before doing anything else; it is
your complete assignment. When your work is complete and committed, submit
your result by running:

    %s result submit --summary "<one-line summary>" --commit <commit-oid>

If that command prints a first line beginning with "transient", the run is
not yet ready to accept your result: wait briefly and run the exact same
command again. Any other non-zero exit means the submission was rejected;
read the printed reason before retrying.
`, f.RunID, f.TaskID, f.AttemptID, f.Brief, f.AssignmentPath, f.HOPPath)
}
