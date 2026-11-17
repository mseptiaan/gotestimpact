// Package gitdiff finds the files that changed on the current branch or in
// the working tree.
package gitdiff

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Repo is a git working tree.
type Repo struct {
	// Root is the absolute, symlink-resolved top-level directory.
	Root string
}

// Open returns the repository that contains dir.
func Open(dir string) (*Repo, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository: %w", err)
	}
	root := strings.TrimSpace(out)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return &Repo{Root: root}, nil
}

// DefaultBase guesses the branch a PR would merge into: origin/HEAD, then
// origin/main, origin/master, main and master.
func (r *Repo) DefaultBase() (string, error) {
	if out, err := git(r.Root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" && r.refExists(ref) {
			return ref, nil
		}
	}
	for _, ref := range []string{"origin/main", "origin/master", "main", "master"} {
		if r.refExists(ref) {
			return ref, nil
		}
	}
	return "", errors.New("cannot find a base branch (tried origin/HEAD, origin/main, origin/master, main, master); pass -base")
}

// MergeBase returns the commit where HEAD forked from base.
func (r *Repo) MergeBase(base string) (string, error) {
	if !r.refExists(base) {
		return "", fmt.Errorf("base ref %q not found; in CI fetch it first (e.g. git fetch origin %s)", base, strings.TrimPrefix(base, "origin/"))
	}
	out, err := git(r.Root, "merge-base", base, "HEAD")
	if err != nil {
		return "", fmt.Errorf("%w; if this is a shallow clone, fetch more history (e.g. actions/checkout with fetch-depth: 0)", err)
	}
	return strings.TrimSpace(out), nil
}

// ChangedSince returns the absolute paths of files that differ between rev
// and the working tree (committed, staged and unstaged changes, including
// deletions) plus untracked files that are not ignored. Renames are reported
// as a deletion of the old path and an addition of the new one.
func (r *Repo) ChangedSince(rev string) ([]string, error) {
	diff, err := git(r.Root, "diff", "--name-only", "--no-renames", "-z", rev, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := git(r.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, out := range []string{diff, untracked} {
		for _, p := range strings.Split(out, "\x00") {
			if p == "" {
				continue
			}
			abs := filepath.Join(r.Root, filepath.FromSlash(p))
			if !seen[abs] {
				seen[abs] = true
				files = append(files, abs)
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

// ShortRev abbreviates a commit hash for display.
func (r *Repo) ShortRev(rev string) string {
	out, err := git(r.Root, "rev-parse", "--short", rev)
	if err != nil {
		return rev
	}
	return strings.TrimSpace(out)
}

func (r *Repo) refExists(ref string) bool {
	_, err := git(r.Root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return string(out), nil
}
