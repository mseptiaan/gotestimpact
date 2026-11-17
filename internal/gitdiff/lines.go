package gitdiff

import (
	"bufio"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Hunk is a changed region on the new side of a diff.
type Hunk struct {
	// Start is the first changed line (1-based) and Count the number of
	// changed lines. Count == 0 is a pure deletion between lines Start and
	// Start+1.
	Start, Count int
}

// FileDiff describes how one file changed.
type FileDiff struct {
	// Path is absolute.
	Path string
	// Added is true for new and untracked files: every line is new.
	Added bool
	// Deleted is true when the file no longer exists.
	Deleted bool
	Hunks   []Hunk
}

// LineChangesSince returns line-level changes between rev and the working
// tree, plus untracked files, restricted to the given pathspecs (for example
// "*.go"). It is keyed by absolute path.
func (r *Repo) LineChangesSince(rev string, pathspecs ...string) (map[string]*FileDiff, error) {
	args := []string{
		"-c", "core.quotepath=off",
		"diff", "-U0", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/", rev, "--",
	}
	out, err := git(r.Root, append(args, pathspecs...)...)
	if err != nil {
		return nil, err
	}
	diffs, err := parseUnifiedDiff(out, r.Root)
	if err != nil {
		return nil, err
	}
	untracked, err := git(r.Root, append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, pathspecs...)...)
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(untracked, "\x00") {
		if p == "" {
			continue
		}
		abs := filepath.Join(r.Root, filepath.FromSlash(p))
		diffs[abs] = &FileDiff{Path: abs, Added: true}
	}
	return diffs, nil
}

// parseUnifiedDiff parses `git diff -U0` output.
func parseUnifiedDiff(out, root string) (map[string]*FileDiff, error) {
	diffs := map[string]*FileDiff{}
	var cur *FileDiff
	var oldPath string
	oldLeft, newLeft := 0, 0

	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if oldLeft > 0 || newLeft > 0 {
			// Inside a hunk body; with -U0 there are no context lines.
			switch {
			case strings.HasPrefix(line, "-"):
				oldLeft--
			case strings.HasPrefix(line, "+"):
				newLeft--
			case strings.HasPrefix(line, `\`):
				// "\ No newline at end of file"
			default:
				return nil, fmt.Errorf("unexpected line in diff hunk: %q", line)
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			cur, oldPath = nil, ""
		case strings.HasPrefix(line, "--- "):
			oldPath = diffPath(line[4:], "a/")
		case strings.HasPrefix(line, "+++ "):
			newPath := diffPath(line[4:], "b/")
			fd := &FileDiff{}
			switch {
			case newPath == "":
				fd.Deleted = true
				newPath = oldPath
			case oldPath == "":
				fd.Added = true
			}
			if newPath == "" {
				return nil, fmt.Errorf("cannot determine path in diff header %q", line)
			}
			fd.Path = filepath.Join(root, filepath.FromSlash(newPath))
			diffs[fd.Path] = fd
			cur = fd
		case strings.HasPrefix(line, "@@ "):
			if cur == nil {
				return nil, fmt.Errorf("hunk without file header: %q", line)
			}
			oldStart, oldCount, newStart, newCount, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			_ = oldStart
			cur.Hunks = append(cur.Hunks, Hunk{Start: newStart, Count: newCount})
			oldLeft, newLeft = oldCount, newCount
		}
	}
	return diffs, sc.Err()
}

// diffPath extracts the path from a ---/+++ header value; "" for /dev/null.
func diffPath(s, prefix string) string {
	s = strings.TrimRight(s, "\t")
	if s == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			s = u
		}
	}
	return strings.TrimPrefix(s, prefix)
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@ ...".
func parseHunkHeader(line string) (oldStart, oldCount, newStart, newCount int, err error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, 0, 0, fmt.Errorf("bad hunk header %q", line)
	}
	if oldStart, oldCount, err = parseRange(fields[1][1:]); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("bad hunk header %q: %w", line, err)
	}
	if newStart, newCount, err = parseRange(fields[2][1:]); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("bad hunk header %q: %w", line, err)
	}
	return oldStart, oldCount, newStart, newCount, nil
}

func parseRange(s string) (start, count int, err error) {
	count = 1
	if i := strings.IndexByte(s, ','); i >= 0 {
		if count, err = strconv.Atoi(s[i+1:]); err != nil {
			return 0, 0, err
		}
		s = s[:i]
	}
	start, err = strconv.Atoi(s)
	return start, count, err
}
