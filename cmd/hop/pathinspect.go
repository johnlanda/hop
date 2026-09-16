package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// errPathNotAbsolute refuses a relative path: worktree retirement only
// ever inspects recorded absolute paths.
var errPathNotAbsolute = errors.New("the path is not absolute")

// errPathUnresolvable refuses a path whose canonical form the filesystem
// cannot establish: a `..` after a component that does not exist, a
// symbolic link on the way that dangles or loops, a non-directory used as
// a directory, an unreadable component, or a path that changed while it
// was being resolved. Retirement retains such a checkout; its path is
// never reported absent.
var errPathUnresolvable = errors.New("the path cannot be resolved safely")

// inspectCanonicalPath implements app.PathInspector for worktree
// retirement. It resolves path exactly as the filesystem does — each
// symbolic link before the next component, and `..` from the directory the
// components before it resolved to — and never collapses the spelling
// first, since `link/..` is the parent of the link's target, not the
// link's own parent. It reports whether path itself exists (a symbolic
// link counts as existing, followed or not) and its canonical form:
//   - an existing path has every symbolic link resolved, except that a
//     dangling link leaf names itself under its resolved parent;
//   - a missing path is its deepest existing prefix, resolved, with the
//     missing plain names appended, so a checkout that is already gone
//     still compares equal to git's canonical listing of it.
//
// Any other path is errPathUnresolvable, carrying at most a system error
// and never the path.
func inspectCanonicalPath(path string) (canonical string, exists bool, err error) {
	if !filepath.IsAbs(path) {
		return "", false, errPathNotAbsolute
	}
	info, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		canonical, err = resolveExistingPath(path, info)
		return canonical, err == nil, err
	case errors.Is(statErr, fs.ErrNotExist):
		canonical, err = resolveMissingPath(path)
		return canonical, false, err
	default:
		return "", false, unresolvable(statErr)
	}
}

// resolveExistingPath canonicalizes a path Lstat found.
func resolveExistingPath(path string, info fs.FileInfo) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", unresolvable(err)
	}
	// Only a dangling symbolic link leaf exists without resolving; it names
	// itself under its resolved parent.
	parent, leaf := splitLeaf(path)
	if info.Mode()&fs.ModeSymlink == 0 || !plainName(leaf) {
		return "", errPathUnresolvable
	}
	dir, missing, err := resolveExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	if len(missing) != 0 {
		return "", errPathUnresolvable
	}
	return filepath.Join(dir, leaf), nil
}

// resolveMissingPath canonicalizes a path Lstat did not find: its deepest
// existing prefix, resolved, with the missing names appended. A `..` among
// the missing names has no filesystem answer, and a path whose every
// component now exists changed under the inspection; both are refused.
func resolveMissingPath(path string) (string, error) {
	dir, missing, err := resolveExistingPrefix(path)
	if err != nil {
		return "", err
	}
	names := []string{dir}
	for _, name := range missing {
		switch name {
		case "", ".":
		case "..":
			return "", errPathUnresolvable
		default:
			names = append(names, name)
		}
	}
	if len(names) == 1 {
		return "", errPathUnresolvable
	}
	return filepath.Join(names...), nil
}

// resolveExistingPrefix walks the absolute path's components in order with
// the filesystem's semantics and stops at the first one that does not
// exist. dir is the fully resolved directory the components before it
// name, and missing holds that component and every one after it,
// verbatim; missing is empty when every component exists. `..` steps to
// the parent of the directory resolved so far, which is symlink-free, so
// its lexical parent is its real parent. A component the walk cannot pass
// is errPathUnresolvable.
func resolveExistingPrefix(path string) (dir string, missing []string, err error) {
	components := strings.Split(path, string(filepath.Separator))
	dir = string(filepath.Separator)
	for i, name := range components {
		switch name {
		case "", ".":
			continue
		case "..":
			dir = filepath.Dir(dir)
			continue
		}
		next := filepath.Join(dir, name)
		info, statErr := os.Lstat(next)
		if errors.Is(statErr, fs.ErrNotExist) {
			return dir, components[i:], nil
		}
		if statErr != nil {
			return "", nil, unresolvable(statErr)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// next's parent is resolved, so this resolves the link (and any
			// chain of links) alone; a dangling or looping link cannot be
			// passed.
			if next, err = filepath.EvalSymlinks(next); err != nil {
				return "", nil, unresolvable(err)
			}
			if info, statErr = os.Lstat(next); statErr != nil {
				return "", nil, unresolvable(statErr)
			}
		}
		if !info.IsDir() && i < len(components)-1 {
			return "", nil, errPathUnresolvable
		}
		dir = next
	}
	return dir, nil, nil
}

// splitLeaf splits an absolute path at its last separator without
// cleaning either part: parent keeps its `..` components for the
// filesystem to resolve.
func splitLeaf(path string) (parent, leaf string) {
	i := strings.LastIndexByte(path, filepath.Separator)
	if i <= 0 {
		return string(filepath.Separator), path[i+1:]
	}
	return path[:i], path[i+1:]
}

// plainName reports whether name is a single path component that names an
// entry of its directory.
func plainName(name string) bool {
	return name != "" && name != "." && name != ".."
}

// unresolvable is errPathUnresolvable carrying err's system cause without
// the path a *fs.PathError names.
func unresolvable(err error) error {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		err = pathErr.Err
	}
	return fmt.Errorf("%w: %w", errPathUnresolvable, err)
}
