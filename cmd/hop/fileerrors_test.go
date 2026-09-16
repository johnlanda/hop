package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/system"
	"github.com/johnlanda/hop/internal/app"
)

// pathCanary is a directory name no fixed diagnostic contains: finding it
// on either stream means a path was echoed.
const pathCanary = "credential-like-path-canary"

// TestFileReadFailuresNeverEchoThePath proves every operator-supplied file
// a worker-plumbing verb reads renders pathErrorCategory's fixed category
// on failure: the path — which may carry a credential-like value or
// terminal control bytes — appears on neither stream, and no store call
// is made.
func TestFileReadFailuresNeverEchoThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), pathCanary, "absent.md")
	unreadableDir := filepath.Join(t.TempDir(), pathCanary)
	if err := os.Mkdir(unreadableDir, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, file := range []struct {
		name     string
		path     string
		category string
	}{
		{name: "missing", path: missing, category: "a path element does not exist"},
		{name: "a directory", path: unreadableDir, category: "i/o failure"},
	} {
		for _, tt := range []struct {
			name   string
			run    func([]string, io.Writer, io.Writer, *deps) (int, error)
			env    map[string]string
			args   []string
			prefix string
		}{
			{name: "msg send --file", run: runMsgSend, env: managerEnv(), args: []string{"--to", "manager", "--kind", "question", "--file", file.path}, prefix: "hop msg send: cannot read body file: "},
			{name: "answer --file", run: runAnswer, env: answerEnv(), args: []string{"--run", testRunID, "--file", file.path, "question-1"}, prefix: "hop answer: cannot read body file: "},
			{name: "task create --file", run: runTaskCreate, env: managerEnv(), args: []string{"--title", "t", "--file", file.path}, prefix: "hop task create: cannot read instructions file: "},
			{name: "review submit --reasons-file", run: runReviewSubmit, env: reviewerEnv(), args: []string{"--verdict", "approve", "--subject", strings.Repeat("c", 40), "--reasons-file", file.path}, prefix: "hop review submit: cannot read reasons file: "},
		} {
			t.Run(tt.name+" "+file.name, func(t *testing.T) {
				td := newTestDeps(&fakeController{}, tt.env, t.TempDir())
				var stdout, stderr bytes.Buffer

				code, err := tt.run(tt.args, &stdout, &stderr, td.deps)
				if err != nil {
					t.Fatalf("write error: %v", err)
				}
				if code == exitOK {
					t.Errorf("exit code = %d, want a failure", code)
				}
				if stdout.Len() != 0 {
					t.Errorf("stdout = %q, want nothing", stdout.String())
				}
				if want := tt.prefix + file.category + "\n"; stderr.String() != want {
					t.Errorf("stderr = %q, want %q", stderr.String(), want)
				}
				for name, stream := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
					if strings.Contains(stream, pathCanary) {
						t.Errorf("%s echoes the path: %q", name, stream)
					}
				}
				if len(td.openCalls) != 0 {
					t.Errorf("the store was opened %d times; a file-read failure precedes every store call", len(td.openCalls))
				}
			})
		}
	}
}

// TestArtifactWriteFailureNeverEchoesThePath proves the use-case error of
// a failed file-first artifact write reaches stderr exactly as the
// application renders it: the real artifact store's failure text is
// path-free by construction (internal/adapters/system), so the verb's
// line carries the step and category only.
func TestArtifactWriteFailureNeverEchoesThePath(t *testing.T) {
	writeErr := newPathFreeArtifactFailure(t)
	ctrl := &fakeController{}
	ctrl.sendMessage = func(app.SendMessageRequest) (app.SendMessageResult, error) {
		return app.SendMessageResult{}, writeErr
	}
	td := newTestDeps(ctrl, managerEnv(), t.TempDir())
	var stdout, stderr bytes.Buffer

	code, err := runMsgSend([]string{"--to", "manager", "--kind", "question", "--body", "q?"}, &stdout, &stderr, td.deps)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if code != exitFailure || stdout.Len() != 0 {
		t.Fatalf("code = %d, stdout = %q; want 1 and nothing", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "a path element is not a directory") || strings.Contains(stderr.String(), pathCanary) {
		t.Errorf("stderr = %q, want the category and no path", stderr.String())
	}
}

// newPathFreeArtifactFailure produces a genuine write failure from the
// real artifact store under a canary-named root, wrapped the way
// SendMessage wraps it.
func newPathFreeArtifactFailure(t *testing.T) error {
	t.Helper()
	root := filepath.Join(t.TempDir(), pathCanary)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(root, "runs")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := system.ArtifactStore{}.WriteArtifact(t.Context(), filepath.Join(blocker, "r", "messages", "m"), []byte("body"))
	if err == nil {
		t.Fatal("the artifact write under a file succeeded")
	}
	return fmt.Errorf("app: write message body: %w", err)
}
