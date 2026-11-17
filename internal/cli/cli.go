// Package cli implements the gotestimpact command.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/mseptiaan/gotestimpact/internal/funcgraph"
	"github.com/mseptiaan/gotestimpact/internal/gitdiff"
	"github.com/mseptiaan/gotestimpact/internal/graph"
	"github.com/mseptiaan/gotestimpact/internal/impact"
)

const usage = `gotestimpact runs only the Go tests affected by your changes.

Usage:
  gotestimpact [flags] [packages] [-- go test flags]

It collects the files changed since the merge-base with the base branch
(committed, staged, unstaged and untracked) and finds the tests that can
reach the changed code. By default it selects individual test functions:
it type-checks the affected packages, builds a reference graph between
declarations and runs 'go test -run' with only the tests that reach a
changed function, method, type or variable. With -by=package it runs every
test of each package whose test binary depends on a changed package.
Packages default to ./... and everything after -- is passed to 'go test'.

Examples:
  gotestimpact                          # changes on this branch vs origin/main
  gotestimpact -base origin/develop -- -race -count=1
  gotestimpact -uncommitted -- -v       # only what you have not committed yet
  gotestimpact -list -explain           # show what would run and why
  gotestimpact -by=package              # whole packages, faster analysis
  git diff --name-only A B | gotestimpact -files-from -

Flags:
`

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type config struct {
	dir             string
	by              string
	base            string
	uncommitted     bool
	filesFrom       string
	tags            string
	list            bool
	listJSON        bool
	explain         bool
	quiet           bool
	runAllOn        stringList
	ignore          stringList
	noDefaultIgnore bool
	version         bool
	patterns        []string
	testArgs        []string
}

// Run executes the command and returns the process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfg, code, ok := parseArgs(args, stderr)
	if !ok {
		return code
	}
	if cfg.version {
		fmt.Fprintln(stdout, "gotestimpact", version())
		return 0
	}
	code, err := run(cfg, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "gotestimpact:", err)
		if code == 0 {
			code = 2
		}
	}
	return code
}

func parseArgs(args []string, stderr io.Writer) (*config, int, bool) {
	cfg := &config{}
	ours := args
	for i, a := range args {
		if a == "--" {
			ours, cfg.testArgs = args[:i], args[i+1:]
			break
		}
	}

	fs := flag.NewFlagSet("gotestimpact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&cfg.dir, "C", ".", "run as if started in `dir`")
	fs.StringVar(&cfg.by, "by", "func", "selection granularity: `func` (test functions) or package")
	fs.StringVar(&cfg.base, "base", "", "base `ref` to diff against (default: origin/HEAD, origin/main, origin/master, main or master)")
	fs.BoolVar(&cfg.uncommitted, "uncommitted", false, "only consider changes not committed yet (diff against HEAD)")
	fs.StringVar(&cfg.filesFrom, "files-from", "", "read changed files from `file` (one per line, - for stdin) instead of git")
	fs.StringVar(&cfg.tags, "tags", "", "build `tags` for go list and go test")
	fs.BoolVar(&cfg.list, "list", false, "print affected packages, one per line followed by the selected tests, and exit")
	fs.BoolVar(&cfg.listJSON, "list-json", false, "print the full analysis as JSON and exit")
	fs.BoolVar(&cfg.explain, "explain", false, "explain why each package was selected")
	fs.BoolVar(&cfg.quiet, "q", false, "do not print the summary")
	fs.Var(&cfg.runAllOn, "run-all-on", "run every test when a changed file matches this `glob` (repeatable; added to "+strings.Join(impact.DefaultRunAllOn, ", ")+")")
	fs.Var(&cfg.ignore, "ignore", "ignore changed files matching this `glob` (repeatable; added to "+strings.Join(impact.DefaultIgnore, ", ")+")")
	fs.BoolVar(&cfg.noDefaultIgnore, "no-default-ignore", false, "do not apply the default ignore patterns")
	fs.BoolVar(&cfg.version, "version", false, "print the version and exit")

	if err := fs.Parse(ours); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0, false
		}
		return nil, 2, false
	}
	cfg.patterns = fs.Args()
	if len(cfg.patterns) == 0 {
		cfg.patterns = []string{"./..."}
	}
	if cfg.by != "func" && cfg.by != "package" {
		fmt.Fprintf(stderr, "gotestimpact: -by must be func or package, not %q\n", cfg.by)
		return nil, 2, false
	}
	if cfg.uncommitted && (cfg.base != "" || cfg.filesFrom != "") {
		fmt.Fprintln(stderr, "gotestimpact: -uncommitted cannot be combined with -base or -files-from")
		return nil, 2, false
	}
	return cfg, 0, true
}

func run(cfg *config, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	ctx := context.Background()
	dir, err := filepath.Abs(cfg.dir)
	if err != nil {
		return 2, err
	}
	logf := func(format string, a ...any) {
		if !cfg.quiet {
			fmt.Fprintf(stderr, "gotestimpact: "+format+"\n", a...)
		}
	}

	moduleRoot, err := findModuleRoot(dir)
	if err != nil {
		return 2, err
	}

	cs, err := changedFiles(cfg, dir, stdin)
	if err != nil {
		return 2, err
	}
	logf("%s: %d changed file(s)", cs.source, len(cs.files))

	g, err := graph.Load(ctx, graph.LoadOptions{Dir: dir, Tags: cfg.tags, Patterns: cfg.patterns})
	if err != nil {
		return 2, err
	}

	opt := impact.Options{
		ModuleRoot: moduleRoot,
		RunAllOn:   append(append([]string{}, impact.DefaultRunAllOn...), cfg.runAllOn...),
		Ignore:     append([]string{}, cfg.ignore...),
	}
	if !cfg.noDefaultIgnore {
		opt.Ignore = append(opt.Ignore, impact.DefaultIgnore...)
	}
	res := impact.Analyze(g, cs.files, opt)

	byFunc := cfg.by == "func"
	if byFunc && hasRunFlag(cfg.testArgs) {
		logf("-run passed to go test, selecting whole packages")
		byFunc = false
	}
	if byFunc && !res.RunAll && len(res.Selected) > 0 {
		var diffs map[string]*gitdiff.FileDiff
		if cs.repo != nil {
			if diffs, err = cs.repo.LineChangesSince(cs.rev, "*.go"); err != nil {
				return 2, err
			}
		}
		toLoad := impact.PackagesToLoad(g, res)
		logf("type-checking %d package(s) to select test functions", len(toLoad))
		fg, err := funcgraph.Load(ctx, funcgraph.Config{
			Dir:      dir,
			Tags:     cfg.tags,
			Packages: toLoad,
			InModule: func(path string) bool {
				if _, ok := g.Packages[path]; ok {
					return true
				}
				_, ok := g.Packages[strings.TrimSuffix(path, "_test")]
				return ok
			},
		})
		if err != nil {
			return 2, err
		}
		impact.Refine(res, g, fg, diffs, moduleRoot)
	}

	if cfg.listJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(res)
	}

	summarize(logf, res)
	if cfg.explain {
		explain(stderr, res)
	}

	if cfg.list {
		for _, s := range res.Selected {
			fmt.Fprintln(stdout, strings.Join(append([]string{s.Package}, s.Tests...), " "))
		}
		return 0, nil
	}
	if len(res.Selected) == 0 {
		logf("no affected tests, nothing to run")
		return 0, nil
	}

	base := []string{"test"}
	if cfg.tags != "" {
		base = append(base, "-tags="+cfg.tags)
	}
	base = append(base, cfg.testArgs...)
	if res.RunAll {
		return goTest(dir, append(base, cfg.patterns...), stdout, stderr)
	}

	var full, filtered, names []string
	seen := map[string]bool{}
	for _, s := range res.Selected {
		if len(s.Tests) == 0 {
			full = append(full, s.Package)
			continue
		}
		filtered = append(filtered, s.Package)
		for _, n := range s.Tests {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	code := 0
	if len(full) > 0 {
		c, err := goTest(dir, append(append([]string{}, base...), full...), stdout, stderr)
		if err != nil {
			return c, err
		}
		code = c
	}
	if len(filtered) > 0 {
		sort.Strings(names)
		args := append(append([]string{}, base...), "-run", "^("+strings.Join(names, "|")+")$")
		c, err := goTest(dir, append(args, filtered...), stdout, stderr)
		if err != nil {
			return c, err
		}
		if code == 0 {
			code = c
		}
	}
	return code, nil
}

func summarize(logf func(string, ...any), res *impact.Result) {
	switch {
	case res.RunAll:
		logf("%s, selecting all %d test package(s)", res.RunAllReason, res.TotalTestPackages)
	case res.ByFunction:
		funcs, full := 0, 0
		for _, s := range res.Selected {
			if len(s.Tests) == 0 {
				full++
			}
			funcs += len(s.Tests)
		}
		logf("%d changed package(s): %d test function(s) in %d package(s), all tests in %d package(s), %d of %d test package(s) skipped",
			len(res.Changed), funcs, len(res.Selected)-full, full, res.TotalTestPackages-len(res.Selected), res.TotalTestPackages)
	default:
		logf("%d changed package(s), %d of %d test package(s) affected", len(res.Changed), len(res.Selected), res.TotalTestPackages)
	}
}

// hasRunFlag reports whether go test arguments already select tests by name.
func hasRunFlag(args []string) bool {
	for _, a := range args {
		a = strings.TrimLeft(a, "-")
		if a == "run" || strings.HasPrefix(a, "run=") || a == "test.run" || strings.HasPrefix(a, "test.run=") {
			return true
		}
	}
	return false
}

type changeSet struct {
	files  []string
	source string
	repo   *gitdiff.Repo // nil when files come from -files-from
	rev    string
}

func changedFiles(cfg *config, dir string, stdin io.Reader) (*changeSet, error) {
	if cfg.filesFrom != "" {
		files, source, err := readFileList(cfg.filesFrom, dir, stdin)
		return &changeSet{files: files, source: source}, err
	}
	repo, err := gitdiff.Open(dir)
	if err != nil {
		return nil, err
	}
	cs := &changeSet{repo: repo, rev: "HEAD", source: "uncommitted changes"}
	if !cfg.uncommitted {
		base := cfg.base
		if base == "" {
			if base, err = repo.DefaultBase(); err != nil {
				return nil, err
			}
		}
		if cs.rev, err = repo.MergeBase(base); err != nil {
			return nil, err
		}
		cs.source = fmt.Sprintf("changes since %s (merge-base %s)", base, repo.ShortRev(cs.rev))
	}
	cs.files, err = repo.ChangedSince(cs.rev)
	return cs, err
}

// readFileList reads newline-separated paths. Relative paths are resolved
// against the git repository root when there is one (matching the output of
// `git diff --name-only`), otherwise against dir.
func readFileList(name, dir string, stdin io.Reader) ([]string, string, error) {
	r := stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		r = f
	}
	base := dir
	if repo, err := gitdiff.Open(dir); err == nil {
		base = repo.Root
	} else if d, err := filepath.EvalSymlinks(dir); err == nil {
		base = d
	}
	var files []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(base, filepath.FromSlash(line))
		}
		files = append(files, line)
	}
	src := "files from " + name
	if name == "-" {
		src = "files from stdin"
	}
	return files, src, sc.Err()
}

func findModuleRoot(dir string) (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("%s is not inside a Go module", dir)
	}
	root := filepath.Dir(gomod)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return root, nil
}

func goTest(dir string, args []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Ctrl-C reaches the go command through the process group; keep running
	// so we can return its exit code.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 2, err
	}
	return 0, nil
}

func explain(w io.Writer, res *impact.Result) {
	if len(res.Changed) > 0 {
		fmt.Fprintln(w, "changed packages:")
		for _, pkg := range sortedKeys(res.Changed) {
			fmt.Fprintf(w, "  %s\n", pkg)
			for _, f := range res.Changed[pkg] {
				fmt.Fprintf(w, "      %s\n", f)
			}
		}
	}
	if len(res.Selected) > 0 {
		fmt.Fprintln(w, "selected test packages:")
		for _, s := range res.Selected {
			var why []string
			if res.RunAll {
				why = append(why, "run all")
			}
			if s.Direct {
				why = append(why, "changed")
			}
			if len(s.Via) > 0 {
				via := s.Via
				more := ""
				if len(via) > 3 {
					more = fmt.Sprintf(" (+%d more)", len(via)-3)
					via = via[:3]
				}
				why = append(why, "depends on "+strings.Join(via, ", ")+more)
			}
			fmt.Fprintf(w, "  %s  [%s]\n", s.Package, strings.Join(why, "; "))
			switch {
			case len(s.Tests) > 0:
				fmt.Fprintf(w, "      run: %s\n", strings.Join(s.Tests, " "))
			case s.AllReason != "":
				fmt.Fprintf(w, "      run: all tests (%s)\n", s.AllReason)
			case res.ByFunction && !res.RunAll:
				fmt.Fprintln(w, "      run: all tests")
			}
		}
	}
	if len(res.DroppedPackages) > 0 {
		fmt.Fprintln(w, "skipped (no test reaches the changed code):")
		for _, p := range res.DroppedPackages {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}
	if len(res.Ignored) > 0 {
		fmt.Fprintln(w, "ignored files:")
		for _, f := range res.Ignored {
			fmt.Fprintf(w, "  %s  (%s)\n", f.Path, f.Reason)
		}
	}
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}
