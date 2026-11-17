package impact

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mseptiaan/gotestimpact/internal/graph"
)

const root = "/repo"

// testGraph models:
//
//	m/a          no tests
//	m/b          imports a, has tests
//	m/c          independent, has tests, embeds static/index.html
//	m/d          xtest imports a only from its tests
//	m/e          has tests reading testdata/
//	m/e/sub      no package files around here except deeper
func testGraph() *graph.Graph {
	pkg := func(path, dir string, files ...string) *graph.Package {
		p := &graph.Package{ImportPath: path, Dir: filepath.Join(root, dir)}
		for _, f := range files {
			p.Files = append(p.Files, filepath.Join(root, dir, f))
		}
		return p
	}
	set := func(s ...string) map[string]bool {
		m := map[string]bool{}
		for _, x := range s {
			m[x] = true
		}
		return m
	}
	return &graph.Graph{
		Packages: map[string]*graph.Package{
			"m":   pkg("m", "", "main.go"),
			"m/a": pkg("m/a", "a", "a.go", "a_linux.go"),
			"m/b": pkg("m/b", "b", "b.go", "b_test.go"),
			"m/c": pkg("m/c", "c", "c.go", "c_test.go", "static/index.html"),
			"m/d": pkg("m/d", "d", "d.go", "d_test.go"),
			"m/e": pkg("m/e", "e", "e.go", "e_test.go"),
		},
		TestDeps: map[string]map[string]bool{
			"m/b": set("m/b", "m/a"),
			"m/c": set("m/c"),
			"m/d": set("m/d", "m/a"),
			"m/e": set("m/e"),
		},
	}
}

func analyze(t *testing.T, files ...string) *Result {
	t.Helper()
	abs := make([]string, len(files))
	for i, f := range files {
		abs[i] = filepath.Join(root, f)
	}
	return Analyze(testGraph(), abs, Options{
		ModuleRoot:   root,
		RunAllOn:     DefaultRunAllOn,
		Ignore:       DefaultIgnore,
		IsModuleRoot: func(dir string) bool { return dir == filepath.Join(root, "tools") },
	})
}

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		want    []string
		runAll  bool
		ignored []string
	}{
		{"transitive and test-only deps", []string{"a/a.go"}, []string{"m/b", "m/d"}, false, nil},
		{"file excluded by build tags", []string{"a/a_linux.go"}, []string{"m/b", "m/d"}, false, nil},
		{"direct change", []string{"c/c.go"}, []string{"m/c"}, false, nil},
		{"test file change", []string{"b/b_test.go"}, []string{"m/b"}, false, nil},
		{"embedded file", []string{"c/static/index.html"}, []string{"m/c"}, false, nil},
		{"testdata walks up to package", []string{"e/testdata/golden/x.json"}, []string{"m/e"}, false, nil},
		{"deleted go file in existing package", []string{"c/old.go"}, []string{"m/c"}, false, nil},
		{"package without tests selects nothing itself", []string{"main.go"}, nil, false, nil},
		{"markdown ignored", []string{"b/README.md"}, nil, false, []string{"b/README.md"}},
		{"embedded markdown is not ignored", []string{"c/static/index.html", "README.md"}, []string{"m/c"}, false, []string{"README.md"}},
		{"nested module", []string{"tools/x/x.go"}, nil, false, []string{"tools/x/x.go"}},
		{"go.mod runs everything", []string{"go.mod", "c/c.go"}, []string{"m/b", "m/c", "m/d", "m/e"}, true, nil},
		{"multiple changes merge", []string{"a/a.go", "e/e.go"}, []string{"m/b", "m/d", "m/e"}, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := analyze(t, tt.files...)
			if got := res.Packages(); !reflect.DeepEqual(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
				t.Errorf("selected = %v, want %v", got, tt.want)
			}
			if res.RunAll != tt.runAll {
				t.Errorf("runAll = %v, want %v", res.RunAll, tt.runAll)
			}
			var ignored []string
			for _, f := range res.Ignored {
				ignored = append(ignored, f.Path)
			}
			if !reflect.DeepEqual(ignored, tt.ignored) {
				t.Errorf("ignored = %v, want %v", ignored, tt.ignored)
			}
		})
	}
}

func TestAnalyzeOutsideModule(t *testing.T) {
	res := Analyze(testGraph(), []string{"/elsewhere/x.go", "/repo-other/y.go"}, Options{ModuleRoot: root})
	if len(res.Selected) != 0 || len(res.Ignored) != 2 {
		t.Fatalf("got selected=%v ignored=%v", res.Selected, res.Ignored)
	}
	for _, f := range res.Ignored {
		if f.Reason != "outside the module" {
			t.Errorf("%s: reason %q", f.Path, f.Reason)
		}
	}
}

func TestAnalyzeReasons(t *testing.T) {
	res := analyze(t, "a/a.go", "b/b.go")
	want := []Selection{
		{Package: "m/b", Direct: true, Via: []string{"m/a"}},
		{Package: "m/d", Via: []string{"m/a"}},
	}
	if !reflect.DeepEqual(res.Selected, want) {
		t.Errorf("selected = %+v, want %+v", res.Selected, want)
	}
	if got := res.Changed["m/a"]; !reflect.DeepEqual(got, []string{"a/a.go"}) {
		t.Errorf("changed[m/a] = %v", got)
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false},
		{"**/go.mod", "sub/go.mod", true},
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/a/b.md", true},
		{"**/*.md", "docs/a/b.mdx", false},
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", ".githubx/ci.yml", false},
		{"LICENSE*", "LICENSE.txt", true},
		{"api/**/*.proto", "api/v1/x.proto", true},
		{"api/**/*.proto", "api/x.proto", true},
		{"api/**/*.proto", "other/x.proto", false},
		{"[", "[", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}
