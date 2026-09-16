package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// errPathNotAbsolute refuses a relative path: worktree retirement only
// ever inspects recorded absolute paths.
var errPathNotAbsolute = errors.New("the path is not absolute")

// inspectCanonicalPath implements app.PathInspector for worktree
// retirement: it reports whether path itself exists (a symbolic link counts
// as existing, followed or not) and its canonical form — every symbolic
// link resolved when the whole path resolves, otherwise the deepest
// existing ancestor resolved with the missing remainder appended, so a
// checkout that is already gone still compares equal to git's canonical
// listing of it. The application never echoes its errors, which may carry
// the path.
func inspectCanonicalPath(path string) (canonical string, exists bool, err error) {
	if !filepath.IsAbs(path) {
		return "", false, errPathNotAbsolute
	}
	path = filepath.Clean(path)
	if _, statErr := os.Lstat(path); statErr == nil {
		exists = true
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", false, statErr
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		return resolved, exists, nil
	} else if !errors.Is(resolveErr, fs.ErrNotExist) {
		return "", false, resolveErr
	}
	// Some element is missing (or the leaf is a dangling link): resolve the
	// deepest ancestor that exists and append the rest verbatim.
	remainder := filepath.Base(path)
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		resolved, resolveErr := filepath.EvalSymlinks(dir)
		if resolveErr == nil {
			return filepath.Join(resolved, remainder), exists, nil
		}
		if !errors.Is(resolveErr, fs.ErrNotExist) {
			return "", false, resolveErr
		}
		if dir == filepath.Dir(dir) {
			return "", false, resolveErr
		}
		remainder = filepath.Join(filepath.Base(dir), remainder)
	}
}
