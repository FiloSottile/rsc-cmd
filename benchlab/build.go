// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Building test binaries.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// build builds all the test binaries needed for the benchmarks,
// using a fresh git worktree per commit so the user's working tree
// is never modified. It writes them to a .benchlab subdirectory.
func (l *Lab) build() error {
	// Using mkdir instead of os.MkdirAll for easier replacement in tests.
	if _, err := l.runLocal(0, "mkdir", "-p", ".benchlab"); err != nil {
		return err
	}

	// Warn about uncommitted changes: benchlab benchmarks resolved commits
	// (HEAD by default), not the working tree, and forgetting to commit
	// before running benchlab is an easy mistake to make.
	dirty, err := l.gitDirty()
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		l.log.Printf("warning: uncommitted changes will not be benchmarked:\n\t%s", strings.Join(dirty, "\n\t"))
	}

	// Subdirectory of the repo where benchlab was invoked,
	// so we can build from the equivalent location in each worktree.
	prefix, err := l.gitPrefix()
	if err != nil {
		return err
	}

	var mu sync.Mutex
	l.built = make(map[commitBuild]*exe)
	for _, commit := range l.Commits {
		workdir, err := l.gitWorktreeAdd(commit)
		if err != nil {
			return err
		}
		if err := l.prepareWorktree(workdir); err != nil {
			l.gitWorktreeRemove(workdir)
			return err
		}
		builddir := filepath.Join(workdir, prefix)
		perr := parDo(l, l.builds, func(b *build) error {
			exe, err := l.buildAt(builddir, workdir, commit, b)
			if err != nil {
				return err
			}
			mu.Lock()
			l.built[commitBuild{commit, b}] = exe
			mu.Unlock()
			return nil
		})
		if rerr := l.gitWorktreeRemove(workdir); rerr != nil {
			l.log.Print(rerr)
		}
		if perr != nil {
			return fmt.Errorf("builds failed")
		}
	}
	return nil
}

// prepareWorktree readies a freshly created worktree for "go test -c".
// For the standard library, that means making the toolchain (bin/) and
// prebuilt packages (pkg/) reachable from inside the worktree.
func (l *Lab) prepareWorktree(workdir string) error {
	if !l.stdlib {
		return nil
	}
	for _, name := range []string{"bin", "pkg"} {
		if err := os.Symlink(filepath.Join(l.root, name), filepath.Join(workdir, name)); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lab) buildAt(dir, workdir, commit string, b *build) (*exe, error) {
	rel := ".benchlab/benchlab." + hash(commit, b.goos, b.goarch, b.env, b.flags) + ".exe"
	// Output path must be absolute because the build command runs with dir as cwd.
	name, err := filepath.Abs(rel)
	if err != nil {
		return nil, err
	}

	// Build binary.
	cmd := []string{"WD=" + dir, "GOOS=" + b.goos, "GOARCH=" + b.goarch}
	if l.stdlib {
		// Without GOROOT set, the go binary resolves GOROOT to l.root
		// (either via its compile-time value or by resolving symlinks),
		// which would build against the user's checkout, not the worktree.
		cmd = append(cmd, "GOROOT="+workdir)
	}
	cmd = append(cmd, b.env...)
	cmd = append(cmd, l.goCmd, "test", "-c", "-o", name)
	cmd = append(cmd, b.flags...)
	if l.Pkg != "" {
		cmd = append(cmd, l.Pkg)
	}
	if _, err := l.runLocal(0, cmd...); err != nil {
		return nil, err
	}

	// Fetch build ID for binary to use as key in cache.
	id, err := l.runLocal(runTrim, l.goCmd, "tool", "buildid", name)
	if err != nil {
		return nil, err
	}
	id = hash(id) // id is too long and has slashes

	return &exe{name: rel, id: id}, nil
}
