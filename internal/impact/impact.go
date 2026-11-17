// Package impact decides which test packages are affected by a set of
// changed files.
package impact

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mseptiaan/gotestimpact/internal/graph"
)

// DefaultRunAllOn lists files whose change can affect every package, so all
// tests run. Patterns are relative to the module root.
var DefaultRunAllOn = []string{"go.mod", "go.sum", "go.work", "go.work.sum", "vendor/modules.txt"}

// DefaultIgnore lists files that never affect tests unless the go tool
// reports them as part of a package (for example through //go:embed).
var DefaultIgnore = []string{"**/*.md", ".github/**", "**/.gitignore", "LICENSE*"}

// Options configures Analyze.
type Options struct {
	// ModuleRoot is the absolute, symlink-resolved module directory.
	ModuleRoot string
	// RunAllOn are glob patterns (relative to ModuleRoot, "**" matches any
	// number of directories). A changed file matching one selects every test
	// package.
	RunAllOn []string
	// Ignore are glob patterns for files that are not part of any package
	// and should not select anything.
	Ignore []string
	// IsModuleRoot reports whether dir contains a go.mod. It defaults to a
	// file system check and exists so tests can replace it.
	IsModuleRoot func(dir string) bool
}

// Result describes what was selected and why.
type Result struct {
	RunAll       bool   `json:"runAll"`
	RunAllReason string `json:"runAllReason,omitempty"`
	// Changed maps each directly changed package to the files (relative to
	// the module root) that changed it.
	Changed map[string][]string `json:"changed"`
	// Selected lists the test packages to run, sorted by import path.
	Selected []Selection `json:"selected"`
	// Ignored lists changed files that did not select anything.
	Ignored []IgnoredFile `json:"ignored,omitempty"`
	// TotalTestPackages is the number of packages with tests in the graph.
	TotalTestPackages int `json:"totalTestPackages"`
	// ByFunction is true when Refine narrowed the selection to functions.
	ByFunction bool `json:"byFunction"`
	// DroppedPackages lists test packages selected at package level whose
	// tests turned out not to reach any changed code.
	DroppedPackages []string `json:"droppedPackages,omitempty"`
}

// Selection is a test package chosen to run.
type Selection struct {
	Package string `json:"package"`
	// Direct is true when the package itself changed.
	Direct bool `json:"direct"`
	// Via lists the changed packages its test binary depends on (other
	// than itself).
	Via []string `json:"via,omitempty"`
	// Tests lists the affected test functions when only some of the
	// package's tests need to run (function-level selection). Empty means
	// every test runs.
	Tests []string `json:"tests,omitempty"`
	// AllReason says why every test of the package runs in function mode.
	AllReason string `json:"allReason,omitempty"`
}

// IgnoredFile is a changed file that selected nothing.
type IgnoredFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Packages returns the import paths of the selected packages.
func (r *Result) Packages() []string {
	out := make([]string, len(r.Selected))
	for i, s := range r.Selected {
		out[i] = s.Package
	}
	return out
}

// Analyze maps changed files (absolute paths) to the test packages that
// must run.
func Analyze(g *graph.Graph, changedFiles []string, opt Options) *Result {
	isModuleRoot := opt.IsModuleRoot
	if isModuleRoot == nil {
		isModuleRoot = hasGoMod
	}
	root := opt.ModuleRoot

	fileIndex := map[string][]string{}
	dirIndex := map[string]string{}
	for path, p := range g.Packages {
		dirIndex[p.Dir] = path
		for _, f := range p.Files {
			fileIndex[f] = append(fileIndex[f], path)
		}
	}

	res := &Result{Changed: map[string][]string{}, TotalTestPackages: len(g.TestDeps)}
	mark := func(pkg, rel string) {
		res.Changed[pkg] = append(res.Changed[pkg], rel)
	}
	ignore := func(rel, reason string) {
		res.Ignored = append(res.Ignored, IgnoredFile{Path: rel, Reason: reason})
	}

	for _, f := range changedFiles {
		f = filepath.Clean(f)
		rel, err := filepath.Rel(root, f)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			ignore(f, "outside the module")
			continue
		}
		relSlash := filepath.ToSlash(rel)

		if p := matchAny(opt.RunAllOn, relSlash); p != "" {
			if !res.RunAll {
				res.RunAll = true
				res.RunAllReason = relSlash + " matches run-all pattern " + p
			}
			continue
		}

		if pkgs := fileIndex[f]; len(pkgs) > 0 {
			for _, pkg := range pkgs {
				mark(pkg, relSlash)
			}
			continue
		}
		if p := matchAny(opt.Ignore, relSlash); p != "" {
			ignore(relSlash, "matches ignore pattern "+p)
			continue
		}
		// Not a known package file: a deleted file, testdata, a fixture read
		// at run time, etc. Attribute it to the closest enclosing package.
		pkg, why := enclosingPackage(filepath.Dir(f), root, dirIndex, isModuleRoot)
		if pkg == "" {
			ignore(relSlash, why)
			continue
		}
		mark(pkg, relSlash)
	}

	if res.RunAll {
		for _, pkg := range g.TestPackages() {
			res.Selected = append(res.Selected, Selection{Package: pkg, Direct: len(res.Changed[pkg]) > 0})
		}
		return res
	}

	changedPkgs := make([]string, 0, len(res.Changed))
	for pkg := range res.Changed {
		changedPkgs = append(changedPkgs, pkg)
	}
	sort.Strings(changedPkgs)

	for _, pkg := range g.TestPackages() {
		deps := g.TestDeps[pkg]
		sel := Selection{Package: pkg, Direct: len(res.Changed[pkg]) > 0}
		for _, c := range changedPkgs {
			if c != pkg && deps[c] {
				sel.Via = append(sel.Via, c)
			}
		}
		if sel.Direct || len(sel.Via) > 0 {
			res.Selected = append(res.Selected, sel)
		}
	}
	return res
}

func enclosingPackage(dir, root string, dirIndex map[string]string, isModuleRoot func(string) bool) (string, string) {
	for {
		if pkg, ok := dirIndex[dir]; ok {
			return pkg, ""
		}
		if dir == root {
			return "", "no Go package in or above its directory"
		}
		if isModuleRoot(dir) {
			return "", "belongs to a nested module"
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "no Go package in or above its directory"
		}
		dir = parent
	}
}

func hasGoMod(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

func matchAny(patterns []string, rel string) string {
	for _, p := range patterns {
		if Match(p, rel) {
			return p
		}
	}
	return ""
}
