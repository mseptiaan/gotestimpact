package impact

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/mseptiaan/gotestimpact/internal/funcgraph"
	"github.com/mseptiaan/gotestimpact/internal/gitdiff"
	"github.com/mseptiaan/gotestimpact/internal/graph"
)

// PackagesToLoad returns the packages function-level analysis must read
// from source: the changed packages, every package that depends on one, and
// the selected test packages.
func PackagesToLoad(g *graph.Graph, res *Result) []string {
	set := map[string]bool{}
	for pkg := range res.Changed {
		set[pkg] = true
	}
	for path, p := range g.Packages {
		for _, d := range p.Deps {
			if len(res.Changed[d]) > 0 {
				set[path] = true
				break
			}
		}
	}
	for _, s := range res.Selected {
		set[s.Package] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Refine narrows a package-level result to the test functions that can
// reach changed declarations. diffs holds line-level changes of Go files
// keyed by absolute path; nil means no line information (every changed Go
// file counts as entirely changed).
func Refine(res *Result, g *graph.Graph, fg *funcgraph.Graph, diffs map[string]*gitdiff.FileDiff, moduleRoot string) *funcgraph.Result {
	res.ByFunction = true
	if res.RunAll {
		return nil
	}

	// deep: every test binary linking the package runs in full.
	// own: only the package's own tests run in full.
	deep := map[string]string{}
	own := map[string]string{}
	setReason := func(m map[string]string, pkg, why string) {
		if _, ok := m[pkg]; !ok {
			m[pkg] = why
		}
	}

	var changes []funcgraph.FileChange
	for _, pkg := range sortedKeys(res.Changed) {
		if msg, broken := fg.Broken[pkg]; broken {
			setReason(deep, pkg, "package "+pkg+" could not be type-checked: "+firstLine(msg))
			continue
		}
		p := g.Packages[pkg]
		for _, rel := range res.Changed[pkg] {
			abs := filepath.Join(moduleRoot, filepath.FromSlash(rel))
			isTestFile := strings.HasSuffix(rel, "_test.go")
			switch {
			case strings.HasSuffix(rel, ".go"):
				d := diffs[abs]
				if diffs != nil && d == nil {
					continue // listed by name but no content change (e.g. file mode)
				}
				if !fg.HasFile(abs) || (d != nil && d.Deleted) {
					why := rel + " cannot be analysed (deleted or excluded by build constraints)"
					if isTestFile {
						setReason(own, pkg, why)
					} else {
						setReason(deep, pkg, why)
					}
					continue
				}
				fc := funcgraph.FileChange{Path: abs, Whole: d == nil || d.Added}
				if d != nil {
					for _, h := range d.Hunks {
						fc.Hunks = append(fc.Hunks, funcgraph.Hunk{Start: h.Start, Count: h.Count})
					}
				}
				changes = append(changes, fc)
			case p != nil && p.Embeds[abs]:
				setReason(deep, pkg, "embedded file "+rel+" changed")
			default:
				setReason(own, pkg, rel+" changed (testdata or other non-Go file)")
			}
		}
	}

	fr := fg.Affected(changes)
	for pkg, key := range fr.InitChanged {
		setReason(deep, pkg, "init-time code changed ("+key+")")
	}
	for pkg, msg := range fg.Broken {
		// A package between a change and a test that failed to type-check
		// may hide references; do not trust function-level results past it.
		setReason(deep, pkg, "package "+pkg+" could not be type-checked: "+firstLine(msg))
	}

	var kept []Selection
	for _, s := range res.Selected {
		s.Tests, s.AllReason = nil, ""
		if why, ok := own[s.Package]; ok {
			s.AllReason = why
		}
		if s.AllReason == "" {
			for _, d := range sortedDeps(g.TestDeps[s.Package]) {
				if why, ok := deep[d]; ok {
					s.AllReason = why
					break
				}
			}
		}
		if s.AllReason == "" {
			names, all := fg.Tests(s.Package, fr)
			switch {
			case all:
				s.AllReason = "TestMain is affected"
			case len(names) == 0:
				res.DroppedPackages = append(res.DroppedPackages, s.Package)
				continue
			default:
				s.Tests = names
			}
		}
		kept = append(kept, s)
	}
	res.Selected = kept
	return fr
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedDeps(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
