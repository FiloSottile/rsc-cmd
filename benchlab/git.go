// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Git interactions.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// gitDetect finds the repository root and, in a jj workspace without a
// .git directory (a non-colocated repository, or a workspace created by
// "jj workspace add"), the underlying Git repository to point git at.
func (l *Lab) gitDetect() error {
	out, err := l.runLocal(runTrim, "git", "rev-parse", "--show-toplevel")
	if err == nil {
		l.root = out
		return nil
	}
	root, rootErr := l.runLocal(runTrim, "jj", "root")
	gitDir, gitDirErr := l.runLocal(runTrim, "jj", "git", "root")
	if rootErr != nil || gitDirErr != nil {
		return err // the original git error
	}
	l.root = root
	l.gitDir = gitDir
	return nil
}

// git returns a git command line for the given arguments,
// pointing git at the repository found by gitDetect.
func (l *Lab) git(args ...string) []string {
	if l.gitDir != "" {
		return stringList("git", "-C", l.gitDir, args)
	}
	return stringList("git", args)
}

// detectStdlib detects whether the current repository is the Go standard
// library and, if so, points l.goCmd at the in-tree go binary so that
// "go test -c" uses the toolchain that matches the checked-out source.
func (l *Lab) detectStdlib() error {
	if _, err := l.fs.Stat(filepath.Join(l.root, "lib", "time", "zoneinfo.zip")); err == nil {
		l.stdlib = true
		l.goCmd = filepath.Join(l.root, "bin", "go")
	}
	return nil
}

// gitDirty returns a list of uncommitted changes in the working tree.
// It reports modified tracked files (staged or not) and untracked new ".go" files.
func (l *Lab) gitDirty() ([]string, error) {
	if l.gitDir != "" {
		return l.jjDirty()
	}
	out, err := l.runLocal(0, "git", "status", "--porcelain")
	if err != nil {
		return nil, err
	}

	var dirty []string
	for line := range strings.Lines(out) {
		if len(line) >= 3 && (line[0] == 'M' || line[1] == 'M' ||
			strings.HasPrefix(line, "?? ") && strings.HasSuffix(line, ".go\n")) {
			// Modified file, staged or not.
			dirty = append(dirty, strings.TrimSpace(line[2:]))
		}
	}
	return dirty, nil
}

// gitResolve resolves the l.Commits list to specific commit hashes,
// using either git rev-list (default) or jj log (when -revisions is set).
func (l *Lab) gitResolve() error {
	if l.commitSet && l.Revisions != "" {
		return fmt.Errorf("-commit and -revisions are mutually exclusive")
	}
	if l.Revisions != "" {
		return l.jjResolve()
	}
	if l.gitDir != "" && !l.commitSet {
		// HEAD tracks the colocated workspace (if any), not this one.
		return fmt.Errorf("no default commits in a jj workspace; use -commit or -revisions")
	}

	var commits []string
	for _, commit := range l.Commits {
		args := l.git("rev-list", "--reverse")
		if !strings.Contains(commit, "..") {
			args = append(args, "-n", "1")
		}
		args = append(args, commit)
		out, err := l.runLocal(0, args...)
		if err != nil {
			return fmt.Errorf("git rev-list %s: %v\n%s", commit, err, out)
		}
		for _, hash := range strings.Fields(out) {
			if len(hash) > 11 {
				hash = hash[:11]
			}
			commits = append(commits, hash)
		}
	}
	l.Commits = commits
	return nil
}

// jjDirty returns the files changed in the working-copy commit @,
// where jj snapshots working tree changes; like uncommitted changes
// in git, they are not benchmarked unless @ is one of the commits.
func (l *Lab) jjDirty() ([]string, error) {
	commit, err := l.runLocal(runTrim, "jj", "log", "--no-graph", "-r", "@", "-T", "commit_id.short(11)")
	if err != nil {
		return nil, err
	}
	if slices.Contains(l.Commits, commit) {
		return nil, nil
	}
	out, err := l.runLocal(0, "jj", "diff", "-r", "@", "--summary")
	if err != nil {
		return nil, err
	}
	var dirty []string
	for line := range strings.Lines(out) {
		if len(line) >= 3 {
			dirty = append(dirty, strings.TrimSpace(line[2:]))
		}
	}
	return dirty, nil
}

// jjResolve resolves l.Revisions (a jj revset) to git commit hashes.
func (l *Lab) jjResolve() error {
	out, err := l.runLocal(0, "jj", "log", "--no-graph", "--reversed",
		"-r", l.Revisions, "-T", `commit_id.short(11) ++ "\n"`)
	if err != nil {
		return err
	}
	l.Commits = nil
	for _, hash := range strings.Fields(out) {
		l.Commits = append(l.Commits, hash)
	}
	if len(l.Commits) == 0 {
		return fmt.Errorf("jj log -r %s: no commits", l.Revisions)
	}
	return nil
}

// gitPrefix returns the path of the current working directory
// relative to the repository root, with a trailing slash
// (or "" when the working directory is the repository root).
func (l *Lab) gitPrefix() (string, error) {
	if l.gitDir != "" {
		// EvalSymlinks matches the canonical paths printed by jj.
		wd, err := os.Getwd()
		if err == nil {
			wd, err = filepath.EvalSymlinks(wd)
		}
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(l.root, wd)
		if err != nil {
			return "", err
		}
		if rel == "." {
			return "", nil
		}
		return filepath.ToSlash(rel) + "/", nil
	}
	return l.runLocal(runTrim, "git", "rev-parse", "--show-prefix")
}

// gitWorktreeAdd creates a detached worktree at the given commit and
// returns its absolute path. Any leftover worktree at the same path
// (from a previous interrupted run) is removed first.
func (l *Lab) gitWorktreeAdd(commit string) (string, error) {
	path, err := filepath.Abs(filepath.Join(".benchlab", "worktree."+hash(commit)))
	if err != nil {
		return "", err
	}
	l.runLocal(0, l.git("worktree", "remove", "--force", path)...)
	if _, err := l.runLocal(0, l.git("worktree", "add", "--quiet", "--detach", path, commit)...); err != nil {
		return "", err
	}
	return path, nil
}

// gitWorktreeRemove removes a worktree previously created by gitWorktreeAdd.
func (l *Lab) gitWorktreeRemove(path string) error {
	_, err := l.runLocal(0, l.git("worktree", "remove", "--force", path)...)
	return err
}
