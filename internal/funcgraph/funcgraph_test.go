package funcgraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) (*Graph, string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found")
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir, err := filepath.Abs("testdata/fx")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir)
	g, err := Load(context.Background(), Config{
		Dir:      dir,
		Packages: []string{"ex.com/fx/store", "ex.com/fx/svc"},
		InModule: func(p string) bool { return p == "ex.com/fx" || strings.HasPrefix(p, "ex.com/fx/") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Broken) > 0 {
		t.Fatalf("broken packages: %v", g.Broken)
	}
	return g, dir
}

// changeAt returns a one-line change at the line containing marker.
func changeAt(t *testing.T, dir, marker string) FileChange {
	t.Helper()
	for _, rel := range []string{"store/store.go", "store/store_test.go", "svc/svc.go", "svc/svc_test.go", "svc/x_test.go"} {
		path := filepath.Join(dir, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, marker) {
				return FileChange{Path: path, Hunks: []Hunk{{Start: i + 1, Count: 1}}}
			}
		}
	}
	t.Fatalf("marker %q not found", marker)
	return FileChange{}
}

func affectedTests(g *Graph, r *Result) []string {
	var out []string
	for _, pkg := range []string{"ex.com/fx/store", "ex.com/fx/svc"} {
		names, all := g.Tests(pkg, r)
		if all {
			out = append(out, pkg+".*")
		}
		for _, n := range names {
			out = append(out, strings.TrimPrefix(pkg, "ex.com/fx/")+"."+n)
		}
	}
	sort.Strings(out)
	return out
}

func TestAffected(t *testing.T) {
	g, dir := loadFixture(t)
	tests := []struct {
		marker string
		want   []string
	}{
		{"MARK:get", []string{"store.TestGet", "svc.TestExternalName", "svc.TestName"}},
		{"MARK:upper", []string{"svc.TestSuite", "svc.TestUpper"}},
		{"MARK:string", []string{"svc.TestLabel"}},
		{"MARK:double", []string{"svc.TestApply"}},
		{"MARK:generic", []string{"svc.TestMap"}},
		{"MARK:unused", nil},
		{"MARK:const", []string{"store.TestLetter"}},
		{"MARK:field-note", []string{"store.TestGet", "store.TestJSON", "svc.TestExternalName", "svc.TestName"}},
		{"MARK:test-comment", nil},
		{"MARK:assert", nil},
	}
	for _, tt := range tests {
		t.Run(tt.marker, func(t *testing.T) {
			r := g.Affected([]FileChange{changeAt(t, dir, tt.marker)})
			if got := affectedTests(g, r); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tests = %v, want %v\nchanged keys: %v", got, tt.want, r.Changed)
			}
			if len(r.InitChanged) > 0 {
				t.Errorf("unexpected init change: %v", r.InitChanged)
			}
		})
	}
}

func TestAffectedInit(t *testing.T) {
	g, dir := loadFixture(t)
	r := g.Affected([]FileChange{changeAt(t, dir, "MARK:init")})
	if _, ok := r.InitChanged["ex.com/fx/svc"]; !ok || len(r.InitChanged) != 1 {
		t.Errorf("InitChanged = %v, want ex.com/fx/svc", r.InitChanged)
	}
}

func TestAffectedWholeFileAndDeletion(t *testing.T) {
	g, dir := loadFixture(t)
	r := g.Affected([]FileChange{{Path: filepath.Join(dir, "svc", "x_test.go"), Whole: true}})
	if got, want := affectedTests(g, r), []string{"svc.TestExternalName"}; !reflect.DeepEqual(got, want) {
		t.Errorf("whole file: %v, want %v", got, want)
	}

	// A deletion inside Double's body changes Double.
	c := changeAt(t, dir, "MARK:double")
	c.Hunks[0].Count = 0
	r = g.Affected([]FileChange{c})
	if got, want := affectedTests(g, r), []string{"svc.TestApply"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deletion: %v, want %v", got, want)
	}

	r = g.Affected([]FileChange{{Path: filepath.Join(dir, "nope.go"), Whole: true}})
	if len(r.Unknown) != 1 {
		t.Errorf("Unknown = %v", r.Unknown)
	}
}
