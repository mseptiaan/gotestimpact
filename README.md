# gotestimpact

Run only the Go tests affected by your changes.

On a large codebase `go test ./...` can take a long time, even though a pull
request usually touches a handful of functions. `gotestimpact` looks at what
changed on your branch (and in your working tree), works out which **test
functions** can reach the changed code, and runs just those with
`go test -run`.

```
$ gotestimpact -- -race
gotestimpact: changes since origin/main (merge-base a1b2c3d): 37 changed file(s)
gotestimpact: type-checking 15 package(s) to select test functions
gotestimpact: 8 changed package(s): 86 test function(s) in 4 package(s), all tests in 2 package(s), 98 of 104 test package(s) skipped
ok   example.com/app/internal/repository    1.2s
ok   example.com/app/internal/usecase        0.9s
...
```

## Install

```bash
go install github.com/mseptiaan/gotestimpact@latest
```

Requires Go 1.26+ and git. The projects you analyse can use any Go version
the go command on your PATH supports.

## Usage

```
gotestimpact [flags] [packages] [-- go test flags]
```

Run it from your module root (or pass `-C dir`). `packages` defaults to `./...`
and everything after `--` goes straight to `go test`.

```bash
# Tests affected by this branch, against origin/main (auto-detected)
gotestimpact

# Different base branch, extra go test flags
gotestimpact -base origin/develop -- -race -count=1

# Only what you haven't committed yet
gotestimpact -uncommitted -- -v

# See what would run, and why, without running anything
gotestimpact -list -explain

# Whole packages instead of test functions (faster analysis, no type-checking)
gotestimpact -by=package

# Machine-readable analysis
gotestimpact -list-json

# Bring your own file list (another VCS, a custom diff, ...)
git diff --name-only HEAD~3 | gotestimpact -files-from -
```

| Flag | Description |
| --- | --- |
| `-by func\|package` | Select test functions (default) or whole packages. |
| `-base ref` | Ref to diff against. Default: `origin/HEAD`, then `origin/main`, `origin/master`, `main`, `master`. |
| `-uncommitted` | Only changes not committed yet (diff against `HEAD`). |
| `-files-from file` | Read changed files from a file (`-` for stdin) instead of git. |
| `-tags tags` | Build tags, used for both `go list` and `go test`. |
| `-list` | Print one line per selected package, followed by its selected tests (none means all), and exit. |
| `-list-json` | Print the full analysis as JSON and exit. |
| `-explain` | Print why each package and test was selected (to stderr). |
| `-run-all-on glob` | Run every test when a changed file matches (repeatable). |
| `-ignore glob` | Ignore changed files that match (repeatable). |
| `-no-default-ignore` | Drop the default ignore patterns. |
| `-q` | No summary lines. |
| `-C dir` | Run as if started in `dir`. |

The exit code is `go test`'s exit code; `0` when nothing needs to run, `2` on
usage or setup errors.

## How it works

1. **Changed files.** `git diff --name-only <merge-base>` against the working
   tree, plus untracked files. That covers commits on the branch, staged and
   unstaged edits, new files and deletions.
2. **Files → packages.** `go list -e -test -json ./...` reports every file
   that belongs to each package: Go sources, `_test.go` files, `//go:embed`
   files, files excluded by build tags (`foo_windows.go`), cgo and assembly.
   A changed file that isn't on any of those lists (testdata, a deleted file,
   a fixture read at run time) is attributed to the closest enclosing package
   directory.
3. **Packages → tests.** For every package with tests, `go list -test` gives
   the full dependency list of its test binary, including packages imported
   only from `_test.go` files and from external `package foo_test` tests. A
   test package is selected when it changed itself or depends on a changed
   package, directly or transitively.
4. **Packages → test functions** (default, `-by=func`). The packages
   between the change and the tests are type-checked (`go/packages`), and a
   reference graph is built between top-level declarations: functions,
   methods, types, struct fields, variables and constants. Changed lines
   (from `git diff -U0`) are mapped to the declarations that contain them,
   and a test function is selected when it reaches one of them through the
   graph. Packages none of whose tests reach the change are dropped.
5. **Run.** `go test [your flags] <packages run in full>` and
   `go test [your flags] -run '^(TestA|TestB|...)$' <other packages>`.

### How function-level selection stays safe

Static analysis cannot see every call, so the graph errs on the side of
running more:

- **Calls through interfaces.** A call to `Repo.Get` through an interface
  depends on every method named `Get` with the same number of parameters and
  results, in any type.
- **Calls from outside your code.** When a method matches an interface
  declared outside the module (`String()` for `fmt`, `ServeHTTP` for
  `net/http`, `MarshalJSON`, `Error`, `Unwrap`, ...), changing it selects
  everything that creates a value of that type. Methods in `_test.go` files
  are treated the same way, so testify suites work.
- **Function values and callbacks.** Referring to a function counts as using
  it, so passing `Double` to `Apply` makes the caller depend on `Double`.
- **Struct fields.** Code that reads or writes a field depends on it. Code
  that creates or declares a value of the struct also depends on its layout,
  which covers reflection (`json.Marshal`, database scanning).
- **Init-time code.** Changes reaching `init` functions, blank variables
  with side effects (`var _ = register()`) or blank imports run every test
  in every binary that links the package. Compile-time checks such as
  `var _ I = (*T)(nil)` do not count.
- **Whole package fallback.** A package runs all of its tests when its
  testdata or other non-Go files change, when `TestMain` is affected, when a
  changed `.go` file can't be analysed (deleted, or excluded by build tags),
  and when a package on the way fails to type-check.
- Lines that are blank or contain only a comment are ignored, so comment
  and formatting edits don't select anything. `//go:` directives count.

### Files that select everything

A change to `go.mod`, `go.sum`, `go.work`, `go.work.sum` or
`vendor/modules.txt` can change any package, so these run the whole suite. Add
your own with `-run-all-on`, for example for files that tests across the
module depend on:

```bash
gotestimpact -run-all-on 'infrastructure/migrations/**' -run-all-on '**/*.proto'
```

Globs are relative to the module root; `**` matches any number of
directories.

### Files that are ignored

By default `**/*.md`, `.github/**`, `**/.gitignore` and `LICENSE*` never
select anything, *unless* the go tool lists them as part of a package (an
embedded `README.md` still counts). Files outside the module, in nested
modules, or with no Go package above them are ignored too. `-explain` lists
every ignored file and why.

## Using it in CI

Comparing against the base branch needs its history, so fetch it:

```yaml
# .github/workflows/test.yml
on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go install github.com/mseptiaan/gotestimpact@latest
      - run: gotestimpact -base origin/${{ github.base_ref }} -explain -- -race
```

It's still a good idea to run the full suite on the main branch (on merge or
nightly) to catch anything the analysis can't see.

## What it can't see

The analysis follows Go imports. A test can also depend on things that aren't
imports:

- **Files read at run time from another package's directory**
  (`os.ReadFile("../../fixtures/x.json")`). Changes there select the package
  that owns that directory, not the test that reads it. Use `-run-all-on`.
- **Database migrations, SQL, config files** outside any Go package. Same:
  use `-run-all-on`.
- **External services**, environment variables, generated code that isn't
  checked in.

When in doubt, `-explain` shows exactly what was selected and what was ignored.

With `-by=func`, subtests are not selected individually: a selected
`TestFoo` runs all of its `t.Run` subtests. Calls made only through
reflection by name (outside test files) or through `unsafe`/`go:linkname`
are not followed. If you pass your own `-run` to `go test`, selection falls
back to whole packages.

## Also check: the go test cache

`go test` already caches results of packages whose inputs haven't changed. If
your CI throws away `$(go env GOCACHE)` between runs, persisting it can give a
large speedup on its own, and combines well with `gotestimpact`.

## License

[MIT](LICENSE)
