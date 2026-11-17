package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture is a module "ex.com/m" in a git repo:
//
//	a/        no tests
//	b/        imports a, has tests
//	c/        independent, has tests
//	d/        external test imports a
//	e/        test reads testdata/input.txt
const goMod = "module ex.com/m\n\ngo 1.22\n"

var fixture = map[string]string{
	"go.mod":               goMod,
	"README.md":            "# m\n",
	"a/a.go":               "package a\n\nfunc A() int { return 1 }\n",
	"b/b.go":               "package b\n\nimport \"ex.com/m/a\"\n\nfunc B() int { return a.A() + 1 }\n",
	"b/b_test.go":          "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {\n\tif B() != 2 {\n\t\tt.Fatal(\"B() != 2\")\n\t}\n}\n",
	"c/c.go":               "package c\n\nfunc C() int { return 3 }\n",
	"c/c_test.go":          "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) { _ = C() }\n",
	"d/d.go":               "package d\n",
	"d/d_test.go":          "package d_test\n\nimport (\n\t\"testing\"\n\n\t\"ex.com/m/a\"\n)\n\nfunc TestD(t *testing.T) { _ = a.A() }\n",
	"e/e.go":               "package e\n",
	"e/e_test.go":          "package e\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestE(t *testing.T) {\n\tif _, err := os.ReadFile(\"testdata/input.txt\"); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n",
	"e/testdata/input.txt": "hello\n",
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
}

func setupRepo(t *testing.T) string {
	t.Helper()
	requireTools(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir := t.TempDir()
	for name, content := range fixture {
		write(t, dir, name, content)
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	commitAll(t, dir, "initial")
	gitRun(t, dir, "checkout", "-q", "-b", "feature")
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", msg)
}

func runCLI(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func listPackages(t *testing.T, dir string, args ...string) []string {
	t.Helper()
	code, out, errOut := runCLI(t, "", append([]string{"-C", dir, "-list", "-by=package"}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, errOut)
	}
	return strings.Fields(out)
}

func TestListBranchChanges(t *testing.T) {
	dir := setupRepo(t)
	write(t, dir, "a/a.go", "package a\n\nfunc A() int { return 1 } // changed\n")
	commitAll(t, dir, "change a")

	if got, want := listPackages(t, dir, "-base", "main"), []string{"ex.com/m/b", "ex.com/m/d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("committed change: got %v, want %v", got, want)
	}

	// Uncommitted work is included in the branch diff, and alone with -uncommitted.
	write(t, dir, "c/c.go", "package c\n\nfunc C() int { return 4 }\n")
	if got, want := listPackages(t, dir, "-base", "main"), []string{"ex.com/m/b", "ex.com/m/c", "ex.com/m/d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("branch + working tree: got %v, want %v", got, want)
	}
	if got, want := listPackages(t, dir, "-uncommitted"), []string{"ex.com/m/c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("-uncommitted: got %v, want %v", got, want)
	}
}

func TestListDefaultBase(t *testing.T) {
	dir := setupRepo(t)
	write(t, dir, "c/c_test.go", "package c\n\nimport \"testing\"\n\nfunc TestC2(t *testing.T) {}\n")
	if got, want := listPackages(t, dir), []string{"ex.com/m/c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestListSpecialFiles(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, dir string)
		want []string
	}{
		{"untracked new test file", func(t *testing.T, dir string) {
			write(t, dir, "c/extra_test.go", "package c\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) {}\n")
		}, []string{"ex.com/m/c"}},
		{"testdata", func(t *testing.T, dir string) {
			write(t, dir, "e/testdata/input.txt", "changed\n")
		}, []string{"ex.com/m/e"}},
		{"markdown only", func(t *testing.T, dir string) {
			write(t, dir, "README.md", "# changed\n")
		}, nil},
		{"go.mod", func(t *testing.T, dir string) {
			write(t, dir, "go.mod", goMod+"\n// changed\n")
		}, []string{"ex.com/m/b", "ex.com/m/c", "ex.com/m/d", "ex.com/m/e"}},
		{"deleted file", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "c", "c_test.go")); err != nil {
				t.Fatal(err)
			}
			write(t, dir, "c/new_test.go", "package c\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {}\n")
		}, []string{"ex.com/m/c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupRepo(t)
			tt.edit(t, dir)
			if got := listPackages(t, dir, "-base", "main"); !reflect.DeepEqual(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilesFrom(t *testing.T) {
	dir := setupRepo(t)
	code, out, errOut := runCLI(t, "a/a.go\n\n", "-C", dir, "-list", "-by=package", "-files-from", "-")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got, want := strings.Fields(out), []string{"ex.com/m/b", "ex.com/m/d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRunGoTest(t *testing.T) {
	dir := setupRepo(t)
	write(t, dir, "a/a.go", "package a\n\nfunc A() int { return 2 - 1 }\n")

	code, out, errOut := runCLI(t, "", "-C", dir, "-explain", "-by=package", "--", "-count=1")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"ok  \tex.com/m/b", "ok  \tex.com/m/d"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ex.com/m/c") || strings.Contains(out, "ex.com/m/e") {
		t.Errorf("unaffected package was tested:\n%s", out)
	}
	if !strings.Contains(errOut, "depends on ex.com/m/a") {
		t.Errorf("-explain output missing reason:\n%s", errOut)
	}

	// Breaking a makes b's test fail; the exit code must propagate.
	write(t, dir, "a/a.go", "package a\n\nfunc A() int { return 100 }\n")
	if code, out, _ := runCLI(t, "", "-C", dir, "-q", "--", "-count=1"); code != 1 {
		t.Errorf("exit code = %d, want 1\n%s", code, out)
	}
}

func TestNothingToRun(t *testing.T) {
	dir := setupRepo(t)
	code, out, errOut := runCLI(t, "", "-C", dir)
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if !strings.Contains(errOut, "nothing to run") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestListJSON(t *testing.T) {
	dir := setupRepo(t)
	write(t, dir, "a/a.go", "package a\n\nfunc A() int { return 1 } // x\n")
	code, out, errOut := runCLI(t, "", "-C", dir, "-list-json", "-by=package")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{`"package": "ex.com/m/b"`, `"a/a.go"`, `"totalTestPackages": 4`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON missing %s:\n%s", want, out)
		}
	}
}

func TestBadArgs(t *testing.T) {
	if code, _, _ := runCLI(t, "", "-uncommitted", "-base", "main"); code != 2 {
		t.Errorf("conflicting flags: exit %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "", "-nope"); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "", "-h"); code != 0 {
		t.Errorf("-h: exit %d, want 0", code)
	}
}

func TestNotAModule(t *testing.T) {
	requireTools(t)
	t.Setenv("GOWORK", "off")
	code, _, errOut := runCLI(t, "", "-C", t.TempDir())
	if code != 2 || !strings.Contains(errOut, "not inside a Go module") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// funcFixture extends the base fixture with several tests per package so
// function-level selection has something to choose from.
func funcFixture(t *testing.T) string {
	dir := setupRepo(t)
	write(t, dir, "a/more.go", "package a\n\nfunc Twice(x int) int {\n\treturn x * 2\n}\n\n// Unrelated is not used by any test.\nfunc Unrelated() int {\n\treturn 7\n}\n")
	write(t, dir, "b/more_test.go", "package b\n\nimport (\n\t\"testing\"\n\n\t\"ex.com/m/a\"\n)\n\nfunc TestTwice(t *testing.T) {\n\tif a.Twice(2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n\nfunc TestPlain(t *testing.T) {}\n")
	commitAll(t, dir, "more")
	gitRun(t, dir, "branch", "-f", "main")
	return dir
}

func listFuncs(t *testing.T, dir string, args ...string) []string {
	t.Helper()
	code, out, errOut := runCLI(t, "", append([]string{"-C", dir, "-list"}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, errOut)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestFuncMode(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, dir string)
		want []string
	}{
		{"one function", func(t *testing.T, dir string) {
			write(t, dir, "a/more.go", "package a\n\nfunc Twice(x int) int {\n\treturn x + x\n}\n\n// Unrelated is not used by any test.\nfunc Unrelated() int {\n\treturn 7\n}\n")
		}, []string{"ex.com/m/b TestTwice"}},
		{"unused function", func(t *testing.T, dir string) {
			write(t, dir, "a/more.go", "package a\n\nfunc Twice(x int) int {\n\treturn x * 2\n}\n\n// Unrelated is not used by any test.\nfunc Unrelated() int {\n\treturn 8\n}\n")
		}, nil},
		{"comment only", func(t *testing.T, dir string) {
			write(t, dir, "a/more.go", "package a\n\n// Twice doubles x.\nfunc Twice(x int) int {\n\treturn x * 2\n}\n\n// Unrelated is not used by any test.\nfunc Unrelated() int {\n\treturn 7\n}\n")
		}, nil},
		{"function used by two packages", func(t *testing.T, dir string) {
			write(t, dir, "a/a.go", "package a\n\nfunc A() int { return 2 - 1 }\n")
		}, []string{"ex.com/m/b TestB", "ex.com/m/d TestD"}},
		{"testdata runs the whole package", func(t *testing.T, dir string) {
			write(t, dir, "e/testdata/input.txt", "changed\n")
		}, []string{"ex.com/m/e"}},
		{"new test file", func(t *testing.T, dir string) {
			write(t, dir, "c/new_test.go", "package c\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {}\n")
		}, []string{"ex.com/m/c TestNew"}},
		{"deleted source file", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "a", "more.go")); err != nil {
				t.Fatal(err)
			}
			write(t, dir, "b/more_test.go", "package b\n\nimport \"testing\"\n\nfunc TestPlain(t *testing.T) {}\n")
		}, []string{"ex.com/m/b", "ex.com/m/d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := funcFixture(t)
			tt.edit(t, dir)
			if got := listFuncs(t, dir, "-base", "main"); !reflect.DeepEqual(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFuncModeRunsOnlySelectedTests(t *testing.T) {
	dir := funcFixture(t)
	write(t, dir, "a/more.go", "package a\n\nfunc Twice(x int) int {\n\treturn x + x\n}\n\n// Unrelated is not used by any test.\nfunc Unrelated() int {\n\treturn 7\n}\n")
	write(t, dir, "c/c_test.go", "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) { _ = C() + 0 }\n")

	code, out, errOut := runCLI(t, "", "-C", dir, "-explain", "--", "-count=1", "-v")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"--- PASS: TestTwice", "--- PASS: TestC"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"TestPlain", "TestB ", "TestD", "TestE"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("stdout contains %q:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(errOut, "run: TestTwice") {
		t.Errorf("-explain missing selected tests:\n%s", errOut)
	}

	// A failing selected test must fail the run.
	write(t, dir, "a/more.go", "package a\n\nfunc Twice(x int) int {\n\treturn x * 3\n}\n\n// Unrelated is not used by any test.\nfunc Unrelated() int {\n\treturn 7\n}\n")
	if code, out, _ := runCLI(t, "", "-C", dir, "-q", "--", "-count=1"); code != 1 {
		t.Errorf("exit code = %d, want 1\n%s", code, out)
	}
}

func TestFuncModeRespectsUserRun(t *testing.T) {
	dir := funcFixture(t)
	write(t, dir, "a/a.go", "package a\n\nfunc A() int { return 2 - 1 }\n")
	code, out, errOut := runCLI(t, "", "-C", dir, "--", "-count=1", "-run", "TestB")
	if code != 0 || !strings.Contains(errOut, "selecting whole packages") || !strings.Contains(out, "ex.com/m/b") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}
