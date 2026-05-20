// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Building test binaries.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// Make sure nothing inside .benchlab (worktrees, cached binaries,
	// benchmark output) ever gets accidentally committed.
	if err := l.fs.WriteFile(".benchlab/.gitignore", []byte("*\n"), 0666); err != nil {
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
	l.testdata = make(map[string]string)
	for _, commit := range l.Commits {
		workdir, err := l.gitWorktreeAdd(commit)
		if err != nil {
			return err
		}
		if err := l.prepareWorktree(workdir, commit); err != nil {
			l.gitWorktreeRemove(workdir)
			return err
		}
		// With -rebuild-stdlib we built a fresh toolchain in the worktree;
		// use it so the benchmark is run against a matching compiler.
		goCmd := l.goCmd
		if l.stdlib && l.RebuildStdlib {
			goCmd = filepath.Join(workdir, "bin", "go")
		}
		builddir := filepath.Join(workdir, prefix)
		// Build one binary per rep, each with a distinct -randlayout seed,
		// so reps see different code layouts. Phase 0 reuses seed 1.
		seeds := make([]int, max(1, l.Reps))
		for i := range seeds {
			seeds[i] = i + 1
		}
		perr := parDo(l, l.builds, func(b *build) error {
			for _, s := range seeds {
				exe, err := l.buildAt(goCmd, builddir, workdir, commit, b, s)
				if err != nil {
					return err
				}
				mu.Lock()
				l.built[commitBuild{commit, b, s}] = exe
				mu.Unlock()
			}
			return nil
		})
		if perr == nil {
			if err := l.captureTestdata(goCmd, builddir, workdir, commit); err != nil {
				l.gitWorktreeRemove(workdir)
				return err
			}
		}
		if rerr := l.gitWorktreeRemove(workdir); rerr != nil {
			l.log.Print(rerr)
		}
		if perr != nil {
			return fmt.Errorf("builds failed")
		}
	}
	return nil
}

// withRandLayout returns a copy of flags with -ldflags=-randlayout=seed merged
// in. If flags already contains a "-ldflags" entry, the randlayout option is
// appended to its value rather than emitted as a separate flag (which would
// otherwise clobber the user's ldflags).
func withRandLayout(flags []string, seed int) []string {
	rand := fmt.Sprintf("-randlayout=%d", seed)
	out := slices.Clone(flags)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-ldflags" || out[i] == "--ldflags" {
			out[i+1] = out[i+1] + " " + rand
			return out
		}
	}
	return append(out, "-ldflags", rand)
}

// prepareWorktree readies a freshly created worktree for "go test -c".
// For the standard library, that means either rebuilding the toolchain
// in place (when -rebuild-stdlib is set) or making the existing toolchain
// (bin/) and prebuilt packages (pkg/) reachable from inside the worktree.
func (l *Lab) prepareWorktree(workdir, commit string) error {
	if !l.stdlib {
		return nil
	}
	if l.RebuildStdlib {
		l.log.Printf("building Go toolchain at %s", commit)
		srcDir := filepath.Join(workdir, "src")
		// Clear GOOS/GOARCH so make.bash builds a host toolchain regardless
		// of any cross-compile values inherited from the environment.
		_, err := l.runLocal(0, "WD="+srcDir, "GOOS=", "GOARCH=", filepath.Join(srcDir, "make.bash"))
		return err
	}
	for _, name := range []string{"bin", "pkg"} {
		if err := os.Symlink(filepath.Join(l.root, name), filepath.Join(workdir, name)); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lab) buildAt(goCmd, dir, workdir, commit string, b *build, seed int) (*exe, error) {
	rel := ".benchlab/benchlab." + hash(commit, b.goos, b.goarch, b.env, b.flags, seed) + ".exe"
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
	cmd = append(cmd, goCmd, "test", "-c", "-o", name)
	cmd = append(cmd, withRandLayout(b.flags, seed)...)
	if l.Pkg != "" {
		cmd = append(cmd, l.Pkg)
	}
	if _, err := l.runLocal(0, cmd...); err != nil {
		return nil, err
	}

	// Fetch build ID for binary to use as key in cache.
	id, err := l.runLocal(runTrim, goCmd, "tool", "buildid", name)
	if err != nil {
		return nil, err
	}
	id = hash(id) // id is too long and has slashes

	return &exe{name: rel, id: id}, nil
}

// captureTestdata finds the testdata directory for the package under test
// inside the worktree and tars it for later upload to remote hosts.
func (l *Lab) captureTestdata(goCmd, builddir, workdir, commit string) error {
	pkg := l.Pkg
	if pkg == "" {
		pkg = "."
	}
	cmd := []string{"WD=" + builddir, goCmd, "list", "-json", pkg}
	if l.stdlib {
		cmd = append([]string{"WD=" + builddir, "GOROOT=" + workdir}, cmd[1:]...)
	}
	out, err := l.runLocal(0, cmd...)
	if err != nil {
		return err
	}
	var info struct{ Dir string }
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return fmt.Errorf("go list -json %s: %v", pkg, err)
	}
	td := filepath.Join(info.Dir, "testdata")
	fi, err := l.fs.Stat(td)
	if err != nil || !fi.IsDir() {
		return nil
	}
	tarFile := ".benchlab/testdata." + commit + ".tar.gz"
	if _, err := l.runLocal(0, "tar", "-czf", tarFile, "-C", filepath.Dir(td), "testdata"); err != nil {
		return err
	}
	l.testdata[commit] = tarFile
	l.log.Printf("captured testdata for %s", commit)
	return nil
}
