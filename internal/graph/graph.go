// Package graph loads a Go module's packages and the dependencies of each
// package's test binary using `go list -test`.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Package is a package of the module being analysed.
type Package struct {
	ImportPath string
	// Dir is the absolute, symlink-resolved package directory.
	Dir string
	// Files holds the absolute paths of every file the go tool attributes to
	// the package: sources, tests, embedded files and files excluded by
	// build constraints.
	Files []string
	// Embeds holds the absolute paths of files embedded by non-test code.
	Embeds map[string]bool
	// Deps lists the module packages this package depends on, directly or
	// transitively (not counting test-only imports).
	Deps []string
}

// Graph is the package graph of a module.
type Graph struct {
	// Packages is keyed by import path.
	Packages map[string]*Package
	// TestDeps maps each package that has tests to the set of module
	// packages its test binary depends on, including dependencies that are
	// imported only by its _test.go files.
	TestDeps map[string]map[string]bool
}

// TestPackages returns the sorted import paths of all packages with tests.
func (g *Graph) TestPackages() []string {
	out := make([]string, 0, len(g.TestDeps))
	for p := range g.TestDeps {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// LoadOptions configures Load.
type LoadOptions struct {
	// Dir is where `go list` runs.
	Dir string
	// Tags is passed as -tags.
	Tags string
	// Patterns are the package patterns to load, e.g. ./...
	Patterns []string
}

type listEntry struct {
	ImportPath        string
	Name              string
	Dir               string
	ForTest           string
	Deps              []string
	GoFiles           []string
	CgoFiles          []string
	CFiles            []string
	CXXFiles          []string
	MFiles            []string
	HFiles            []string
	FFiles            []string
	SFiles            []string
	SwigFiles         []string
	SwigCXXFiles      []string
	SysoFiles         []string
	OtherFiles        []string
	EmbedFiles        []string
	TestEmbedFiles    []string
	XTestEmbedFiles   []string
	TestGoFiles       []string
	XTestGoFiles      []string
	IgnoredGoFiles    []string
	IgnoredOtherFiles []string
}

func (e *listEntry) fileLists() [][]string {
	return [][]string{
		e.GoFiles, e.CgoFiles, e.CFiles, e.CXXFiles, e.MFiles, e.HFiles, e.FFiles,
		e.SFiles, e.SwigFiles, e.SwigCXXFiles, e.SysoFiles, e.OtherFiles,
		e.EmbedFiles, e.TestEmbedFiles, e.XTestEmbedFiles,
		e.TestGoFiles, e.XTestGoFiles, e.IgnoredGoFiles, e.IgnoredOtherFiles,
	}
}

var listFields = []string{
	"ImportPath", "Name", "Dir", "ForTest", "Deps",
	"GoFiles", "CgoFiles", "CFiles", "CXXFiles", "MFiles", "HFiles", "FFiles",
	"SFiles", "SwigFiles", "SwigCXXFiles", "SysoFiles", "OtherFiles",
	"EmbedFiles", "TestEmbedFiles", "XTestEmbedFiles",
	"TestGoFiles", "XTestGoFiles", "IgnoredGoFiles", "IgnoredOtherFiles",
}

// Load runs `go list -e -test` and builds the graph.
func Load(ctx context.Context, opt LoadOptions) (*Graph, error) {
	args := []string{"list", "-e", "-test", "-json=" + strings.Join(listFields, ",")}
	if opt.Tags != "" {
		args = append(args, "-tags="+opt.Tags)
	}
	patterns := opt.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	args = append(args, patterns...)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = opt.Dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	g, parseErr := Parse(stdout)
	// Drain so the go command never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("go list: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	if parseErr != nil {
		return nil, fmt.Errorf("parsing go list output: %w", parseErr)
	}
	return g, nil
}

// Parse builds a graph from the JSON stream printed by `go list -test -json`.
func Parse(r io.Reader) (*Graph, error) {
	var base, testMains []*listEntry
	dec := json.NewDecoder(r)
	for {
		e := new(listEntry)
		if err := dec.Decode(e); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		switch {
		case e.Dir == "":
			// Unresolvable import; nothing on disk to map files to.
		case e.ForTest != "":
			// "p [p.test]" and "p_test [p.test]" variants. Their
			// dependencies are already included in the p.test entry.
		case e.Name == "main" && strings.HasSuffix(e.ImportPath, ".test"):
			testMains = append(testMains, e)
		default:
			base = append(base, e)
		}
	}

	g := &Graph{
		Packages: make(map[string]*Package, len(base)),
		TestDeps: make(map[string]map[string]bool, len(testMains)),
	}
	add := func(e *listEntry) {
		dir := canonical(e.Dir)
		p := &Package{ImportPath: e.ImportPath, Dir: dir}
		for _, list := range e.fileLists() {
			for _, f := range list {
				if !filepath.IsAbs(f) {
					f = filepath.Join(dir, filepath.FromSlash(f))
				}
				p.Files = append(p.Files, f)
			}
		}
		for _, f := range e.EmbedFiles {
			if p.Embeds == nil {
				p.Embeds = map[string]bool{}
			}
			p.Embeds[filepath.Join(dir, filepath.FromSlash(f))] = true
		}
		g.Packages[e.ImportPath] = p
	}
	for _, e := range base {
		add(e)
	}
	for _, e := range base {
		p := g.Packages[e.ImportPath]
		for _, d := range e.Deps {
			if _, ok := g.Packages[d]; ok {
				p.Deps = append(p.Deps, d)
			}
		}
	}

	for _, e := range testMains {
		pkg := strings.TrimSuffix(e.ImportPath, ".test")
		p, ok := g.Packages[pkg]
		if !ok || p.Dir != canonical(e.Dir) {
			// A real package whose path happens to end in ".test".
			add(e)
			continue
		}
		deps := map[string]bool{pkg: true}
		for _, d := range e.Deps {
			if i := strings.Index(d, " ["); i >= 0 {
				d = d[:i]
			}
			if _, ok := g.Packages[d]; ok {
				deps[d] = true
			}
		}
		g.TestDeps[pkg] = deps
	}
	return g, nil
}

func canonical(dir string) string {
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		return d
	}
	return filepath.Clean(dir)
}
