package funcgraph

import "sort"

// Hunk is a changed region on the new side of a diff: lines
// [Start, Start+Count), or a pure deletion between Start and Start+1 when
// Count is 0.
type Hunk struct{ Start, Count int }

// FileChange describes a changed Go file.
type FileChange struct {
	// Path is absolute and symlink-resolved.
	Path string
	// Whole marks every declaration in the file as changed (new files, or
	// when no line information is available).
	Whole bool
	Hunks []Hunk
}

// Result is the outcome of Affected.
type Result struct {
	// Changed are the keys of the declarations that changed.
	Changed []string
	// Affected holds every key that can reach a changed declaration.
	Affected map[string]bool
	// InitChanged maps packages whose init-time behaviour may have changed
	// (init functions, blank variables, blank imports) to the reason. Every
	// test binary that links such a package must run in full.
	InitChanged map[string]string
	// Unknown lists changed files that are not part of the graph.
	Unknown []string
}

// Affected marks the declarations touched by changes and everything that
// references them, directly or transitively.
func (g *Graph) Affected(changes []FileChange) *Result {
	res := &Result{Affected: map[string]bool{}, InitChanged: map[string]string{}}
	var queue []string
	mark := func(keys []string) {
		for _, k := range keys {
			if !res.Affected[k] {
				res.Affected[k] = true
				res.Changed = append(res.Changed, k)
				queue = append(queue, k)
			}
		}
	}

	for _, c := range changes {
		fi := g.files[c.Path]
		if fi == nil {
			res.Unknown = append(res.Unknown, c.Path)
			continue
		}
		if c.Whole {
			for _, sp := range fi.spans {
				mark(sp.keys)
				for _, sub := range sp.subs {
					mark(sub.keys)
				}
			}
			continue
		}
		for _, h := range c.Hunks {
			if h.Count == 0 {
				fi.markDeletion(h.Start, mark)
				continue
			}
			for line := h.Start; line < h.Start+h.Count; line++ {
				if line < len(fi.skip) && fi.skip[line] {
					continue
				}
				fi.markLine(line, mark)
			}
		}
	}
	sort.Strings(res.Changed)

	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		for from := range g.rdeps[k] {
			if !res.Affected[from] {
				res.Affected[from] = true
				queue = append(queue, from)
			}
		}
	}

	for k := range res.Affected {
		if pkg, ok := g.initKeys[k]; ok {
			if _, seen := res.InitChanged[pkg]; !seen || k < res.InitChanged[pkg] {
				res.InitChanged[pkg] = k
			}
		}
	}
	return res
}

func (fi *fileInfo) markLine(line int, mark func([]string)) {
	for _, sp := range fi.spans {
		if !sp.contains(line) {
			continue
		}
		for _, sub := range sp.subs {
			if sub.contains(line) {
				mark(sub.keys)
				return
			}
		}
		mark(sp.keys)
		return
	}
}

func (fi *fileInfo) markDeletion(after int, mark func([]string)) {
	for _, sp := range fi.spans {
		if !sp.contains(after) || !sp.contains(after+1) {
			continue
		}
		for _, sub := range sp.subs {
			if sub.contains(after) && sub.contains(after+1) {
				mark(sub.keys)
				return
			}
		}
		if sp.delKeys != nil {
			mark(sp.delKeys)
		} else {
			mark(sp.keys)
		}
		return
	}
}

func (sp *span) contains(line int) bool { return sp.start <= line && line <= sp.end }

// HasFile reports whether path (absolute, symlink-resolved) was analysed.
func (g *Graph) HasFile(path string) bool { return g.files[path] != nil }

// Tests returns the affected tests of package pkg, sorted. all is true when
// TestMain is affected, so every test of the package must run.
func (g *Graph) Tests(pkg string, r *Result) (names []string, all bool) {
	pt := g.tests[pkg]
	if pt == nil {
		return nil, false
	}
	if pt.mainKey != "" && r.Affected[pt.mainKey] {
		return nil, true
	}
	for _, t := range pt.tests {
		if r.Affected[t.Key] {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	return names, false
}
