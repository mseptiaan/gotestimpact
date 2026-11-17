// Package funcgraph builds a reference graph between the top-level
// declarations of a set of packages (functions, methods, types, struct
// fields, variables and constants) and finds the test functions that can
// reach a changed declaration.
//
// Nodes are identified by string keys so that the same declaration seen
// through different package variants (p, "p [p.test]") is one node:
//
//	path.Func            package-level function, variable, constant or type
//	path.Type.Method     method (also interface methods)
//	path.Type.Field      struct field
//	path.Type#fields     "the layout of Type changed" (for reflection users)
//	path.Type#methods    "a dynamically called method of Type changed"
//	virtual:Name/2/1     any method Name with 2 params and 1 result, used
//	                     for calls through interfaces
//	path.init#N          init function (changes force a package-wide run)
package funcgraph

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Config configures Load.
type Config struct {
	// Dir is where the go command runs.
	Dir string
	// Tags are build tags.
	Tags string
	// Packages are the import paths to analyse from source.
	Packages []string
	// InModule reports whether an import path belongs to the module being
	// analysed. References to other packages are not tracked.
	InModule func(path string) bool
}

// TestFunc is a top-level test, example or fuzz target.
type TestFunc struct {
	Name string
	Key  string
}

// Graph is the declaration reference graph.
type Graph struct {
	rdeps map[string]map[string]struct{}
	files map[string]*fileInfo
	tests map[string]*pkgTests
	// initKeys maps init-like nodes (init functions, blank variables,
	// blank imports) to their package.
	initKeys map[string]string
	// Broken maps packages that could not be type-checked to an error.
	Broken map[string]string
}

type pkgTests struct {
	tests   []TestFunc
	mainKey string
}

type fileInfo struct {
	pkg   string
	spans []*span
	skip  []bool // blank or comment-only lines, indexed by line number
}

type span struct {
	start, end int
	keys       []string
	subs       []*span
	// delKeys, when set, are marked instead of keys for a deletion that
	// falls between subs (a removed struct field).
	delKeys []string
}

// Load type-checks cfg.Packages (with their tests) and builds the graph.
func Load(ctx context.Context, cfg Config) (*Graph, error) {
	g := &Graph{
		rdeps:    map[string]map[string]struct{}{},
		files:    map[string]*fileInfo{},
		tests:    map[string]*pkgTests{},
		initKeys: map[string]string{},
		Broken:   map[string]string{},
	}
	if len(cfg.Packages) == 0 {
		return g, nil
	}
	pcfg := &packages.Config{
		Context: ctx,
		Dir:     cfg.Dir,
		Tests:   true,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
	}
	if cfg.Tags != "" {
		pcfg.BuildFlags = []string{"-tags=" + cfg.Tags}
	}
	pkgs, err := packages.Load(pcfg, cfg.Packages...)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	chosen := choosePackages(pkgs)
	external := externalMethods(chosen, cfg.InModule)
	for _, p := range chosen {
		base := basePath(p)
		if len(p.Errors) > 0 || p.TypesInfo == nil {
			if _, ok := g.Broken[base]; !ok {
				msg := "type information unavailable"
				if len(p.Errors) > 0 {
					msg = p.Errors[0].Error()
				}
				g.Broken[base] = msg
			}
			if p.TypesInfo == nil {
				continue
			}
		}
		b := &builder{g: g, pkg: p, base: base, inModule: cfg.InModule, external: external}
		b.build()
	}
	return g, nil
}

// choosePackages keeps one variant per package path: the one compiled with
// its own _test.go files when it exists, plus external test packages.
func choosePackages(pkgs []*packages.Package) []*packages.Package {
	byPath := map[string]*packages.Package{}
	for _, p := range pkgs {
		if p.Name == "main" && strings.HasSuffix(p.ID, ".test") {
			continue // generated test main
		}
		own := p.ID == p.PkgPath || p.ID == p.PkgPath+" ["+basePath(p)+".test]"
		if !own {
			continue // a dependency recompiled for some other test binary
		}
		cur, ok := byPath[p.PkgPath]
		if !ok || len(p.Syntax) > len(cur.Syntax) {
			byPath[p.PkgPath] = p
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]*packages.Package, len(paths))
	for i, path := range paths {
		out[i] = byPath[path]
	}
	return out
}

// basePath returns the package under test for test variants
// ("p_test [p.test]" -> "p") and the package path otherwise.
func basePath(p *packages.Package) string {
	if i := strings.Index(p.ID, " ["); i >= 0 && strings.HasSuffix(p.ID, ".test]") {
		return strings.TrimSuffix(p.ID[i+2:], ".test]")
	}
	return p.PkgPath
}

// knownDynamicMethods are methods the standard library calls through
// interfaces that are not exported as named types (errors.Is and friends).
var knownDynamicMethods = []string{"Unwrap/0/1", "Is/1/1", "As/1/1", "Timeout/0/1", "Temporary/0/1"}

// externalMethods collects the signatures of methods of interfaces declared
// outside the module: a concrete method matching one of them may be called
// by code we cannot see (net/http calling ServeHTTP, fmt calling String).
func externalMethods(pkgs []*packages.Package, inModule func(string) bool) map[string]bool {
	out := map[string]bool{"Error/0/1": true}
	for _, m := range knownDynamicMethods {
		out[m] = true
	}
	seen := map[*types.Package]bool{}
	var visit func(tp *types.Package)
	visit = func(tp *types.Package) {
		if tp == nil || seen[tp] {
			return
		}
		seen[tp] = true
		if !inModule(tp.Path()) {
			scope := tp.Scope()
			for _, name := range scope.Names() {
				tn, ok := scope.Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				iface, ok := tn.Type().Underlying().(*types.Interface)
				if !ok {
					continue
				}
				for i := 0; i < iface.NumMethods(); i++ {
					out[methodSig(iface.Method(i))] = true
				}
			}
		}
		for _, imp := range tp.Imports() {
			visit(imp)
		}
	}
	for _, p := range pkgs {
		visit(p.Types)
	}
	return out
}

func methodSig(f *types.Func) string {
	sig := f.Type().(*types.Signature)
	s := f.Name() + "/" + strconv.Itoa(sig.Params().Len()) + "/" + strconv.Itoa(sig.Results().Len())
	if sig.Variadic() {
		s += "v"
	}
	return s
}

func virtualKey(f *types.Func) string { return "virtual:" + methodSig(f) }

func (g *Graph) addEdge(from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	s := g.rdeps[to]
	if s == nil {
		s = map[string]struct{}{}
		g.rdeps[to] = s
	}
	s[from] = struct{}{}
}

// ---------------------------------------------------------------------------
// Building

type builder struct {
	g        *Graph
	pkg      *packages.Package
	base     string
	inModule func(string) bool
	external map[string]bool
	initN    int
	blankN   int
}

type refSet map[string]struct{}

func (r refSet) add(k string) {
	if k != "" {
		r[k] = struct{}{}
	}
}

func (b *builder) build() {
	fset := b.pkg.Fset
	for _, file := range b.pkg.Syntax {
		name := fset.Position(file.Pos()).Filename
		if name == "" {
			continue
		}
		path := canonicalFile(name)
		fi := &fileInfo{pkg: b.base, skip: skipLines(path)}
		isTest := strings.HasSuffix(name, "_test.go")
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if sp := b.funcDecl(d, isTest); sp != nil {
					fi.spans = append(fi.spans, sp)
				}
			case *ast.GenDecl:
				fi.spans = append(fi.spans, b.genDecl(d)...)
			}
		}
		sort.Slice(fi.spans, func(i, j int) bool { return fi.spans[i].start < fi.spans[j].start })
		b.g.files[path] = fi
	}
}

func (b *builder) line(p token.Pos) int { return b.pkg.Fset.Position(p).Line }

func (b *builder) funcDecl(d *ast.FuncDecl, isTest bool) *span {
	obj, _ := b.pkg.TypesInfo.Defs[d.Name].(*types.Func)
	if obj == nil {
		return nil
	}
	sp := &span{start: b.line(d.Pos()), end: b.line(d.End())}
	var key string
	switch {
	case d.Recv == nil && d.Name.Name == "init":
		key = fmt.Sprintf("%s.init#%d", b.pkg.PkgPath, b.initN)
		b.initN++
		b.g.initKeys[key] = b.base
	default:
		key = b.objKey(obj)
	}
	if key == "" {
		return nil
	}
	sp.keys = []string{key}
	if d.Recv != nil {
		// Whenever this method is affected, so is every call through an
		// interface with a matching method, and, if code outside the module
		// may call it dynamically, every place that creates a value of the
		// receiver type.
		sig := methodSig(obj)
		b.g.addEdge("virtual:"+sig, key)
		// Methods in test files may be run by reflection (testify suites
		// call every TestXxx method), and so may methods matching interfaces
		// declared outside the module.
		if b.external[sig] || isTest {
			if n := namedOf(obj.Type().(*types.Signature).Recv().Type()); n != nil {
				b.g.addEdge(typeKey(n)+"#methods", key)
			}
		}
	}

	refs := refSet{}
	b.collect(d.Type, false, refs)
	if d.Body != nil {
		b.collect(d.Body, true, refs)
	}
	for r := range refs {
		b.g.addEdge(key, r)
	}

	if isTest && d.Recv == nil {
		b.recordTest(d, key)
	}
	return sp
}

func (b *builder) recordTest(d *ast.FuncDecl, key string) {
	pt := b.g.tests[b.base]
	if pt == nil {
		pt = &pkgTests{}
		b.g.tests[b.base] = pt
	}
	name := d.Name.Name
	if name == "TestMain" && d.Type.Params.NumFields() == 1 {
		pt.mainKey = key
		return
	}
	for _, prefix := range []string{"Test", "Example", "Fuzz"} {
		if isTestName(name, prefix) {
			pt.tests = append(pt.tests, TestFunc{Name: name, Key: key})
			return
		}
	}
}

// isTestName mirrors the go tool: "Test", "TestFoo" and "Test_foo" count,
// "Testfoo" does not.
func isTestName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	c := name[len(prefix)]
	return !('a' <= c && c <= 'z')
}

func (b *builder) genDecl(d *ast.GenDecl) []*span {
	var spans []*span
	switch d.Tok {
	case token.IMPORT:
		for _, s := range d.Specs {
			is := s.(*ast.ImportSpec)
			if is.Name != nil && is.Name.Name == "_" {
				key := fmt.Sprintf("%s.import_#%d", b.pkg.PkgPath, b.blankN)
				b.blankN++
				b.g.initKeys[key] = b.base
				spans = append(spans, &span{start: b.line(is.Pos()), end: b.line(is.End()), keys: []string{key}})
			}
		}

	case token.CONST, token.VAR:
		// A parenthesised const block is treated as one unit: iota and
		// implicit repetition tie its values together.
		block := d.Tok == token.CONST && d.Lparen.IsValid()
		var blockSpan *span
		if block {
			blockSpan = &span{start: b.line(d.Pos()), end: b.line(d.End())}
			spans = append(spans, blockSpan)
		}
		for _, s := range d.Specs {
			vs := s.(*ast.ValueSpec)
			var keys []string
			for _, n := range vs.Names {
				if n.Name == "_" {
					key := fmt.Sprintf("%s._#%d", b.pkg.PkgPath, b.blankN)
					b.blankN++
					// "var _ I = (*T)(nil)" is a compile-time check; only
					// initializers that call something run code at init.
					if b.hasCall(vs.Values) {
						b.g.initKeys[key] = b.base
					}
					keys = append(keys, key)
					continue
				}
				keys = append(keys, b.objKey(b.pkg.TypesInfo.Defs[n]))
			}
			refs := refSet{}
			b.collect(vs, true, refs)
			for _, k := range keys {
				for r := range refs {
					b.g.addEdge(k, r)
				}
			}
			if block {
				blockSpan.keys = append(blockSpan.keys, keys...)
				continue
			}
			start := vs.Pos()
			if !d.Lparen.IsValid() {
				start = d.Pos()
			}
			spans = append(spans, &span{start: b.line(start), end: b.line(vs.End()), keys: keys})
		}

	case token.TYPE:
		for _, s := range d.Specs {
			if sp := b.typeSpec(d, s.(*ast.TypeSpec)); sp != nil {
				spans = append(spans, sp)
			}
		}
	}
	return spans
}

func (b *builder) typeSpec(d *ast.GenDecl, ts *ast.TypeSpec) *span {
	key := b.objKey(b.pkg.TypesInfo.Defs[ts.Name])
	if key == "" {
		return nil
	}
	start := ts.Pos()
	if !d.Lparen.IsValid() {
		start = d.Pos()
	}
	sp := &span{start: b.line(start), end: b.line(ts.End()), keys: []string{key}}

	refs := refSet{}
	if ts.TypeParams != nil {
		b.collect(ts.TypeParams, false, refs)
	}
	b.collect(ts.Type, true, refs)
	for r := range refs {
		b.g.addEdge(key, r)
		b.g.addEdge(key+"#fields", r)
	}

	switch t := ts.Type.(type) {
	case *ast.StructType:
		sp.delKeys = []string{key + "#fields"}
		for _, f := range t.Fields.List {
			sub := &span{start: b.line(fieldStart(f)), end: b.line(f.End())}
			if len(f.Names) == 0 {
				// Embedded: its promoted fields and methods become ours.
				sub.keys = append(sub.keys, key+"."+embeddedName(f.Type), key, key+"#methods")
				if n := namedOf(b.pkg.TypesInfo.TypeOf(f.Type)); n != nil {
					if ek := typeKey(n); ek != "" && b.inModule(n.Obj().Pkg().Path()) {
						b.g.addEdge(key+"#methods", ek+"#methods")
					}
				}
			}
			for _, n := range f.Names {
				sub.keys = append(sub.keys, key+"."+n.Name)
			}
			sub.keys = append(sub.keys, key+"#fields")
			sp.subs = append(sp.subs, sub)
		}
	case *ast.InterfaceType:
		for _, m := range t.Methods.List {
			sub := &span{start: b.line(fieldStart(m)), end: b.line(m.End())}
			if len(m.Names) == 0 {
				sub.keys = []string{key} // embedded interface or type union
			}
			for _, n := range m.Names {
				sub.keys = append(sub.keys, key+"."+n.Name)
			}
			sp.subs = append(sp.subs, sub)
		}
	}
	return sp
}

// hasCall reports whether evaluating exprs calls a function (as opposed to
// type conversions and builtins without side effects).
func (b *builder) hasCall(exprs []ast.Expr) bool {
	found := false
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if found {
				return false
			}
			if _, ok := n.(*ast.FuncLit); ok {
				return false // defined, not called
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if tv, ok := b.pkg.TypesInfo.Types[call.Fun]; ok && tv.IsType() {
				return true
			}
			if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
				if bi, ok := b.pkg.TypesInfo.Uses[id].(*types.Builtin); ok {
					switch bi.Name() {
					case "panic", "print", "println", "recover":
					default:
						return true
					}
				}
			}
			found = true
			return false
		})
	}
	return found
}

func fieldStart(f *ast.Field) token.Pos {
	if f.Doc != nil {
		return f.Doc.Pos()
	}
	return f.Pos()
}

func embeddedName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.SelectorExpr:
			return x.Sel.Name
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// collect records the module declarations that n refers to. body is true
// for code that runs (function bodies, initializers, type definitions), as
// opposed to function signatures; only running code that names a type can
// observe its layout or call its methods dynamically.
func (b *builder) collect(n ast.Node, body bool, refs refSet) {
	info := b.pkg.TypesInfo
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			obj := info.Uses[x]
			if obj == nil {
				return true
			}
			if f, ok := obj.(*types.Func); ok {
				if recv := f.Type().(*types.Signature).Recv(); recv != nil && types.IsInterface(recv.Type()) {
					refs.add(virtualKey(f))
				}
			}
			k := b.objKey(obj)
			refs.add(k)
			if _, isType := obj.(*types.TypeName); isType && body && k != "" {
				refs.add(k + "#fields")
				refs.add(k + "#methods")
			}
		case *ast.SelectorExpr:
			if sel := info.Selections[x]; sel != nil {
				b.selectionRefs(sel, refs)
			}
		case *ast.CompositeLit:
			b.compositeRefs(x, refs)
		}
		return true
	})
}

// selectionRefs records the struct fields a selector walks through,
// including embedded fields on the way to a promoted field or method.
func (b *builder) selectionRefs(sel *types.Selection, refs refSet) {
	t := sel.Recv()
	idx := sel.Index()
	for i, ix := range idx {
		if i == len(idx)-1 && sel.Kind() != types.FieldVal {
			return // the method itself is recorded through Uses
		}
		st, ok := deref(t).Underlying().(*types.Struct)
		if !ok || ix >= st.NumFields() {
			return
		}
		f := st.Field(ix)
		if n := namedOf(t); n != nil {
			refs.add(b.fieldKey(n, f.Name()))
		}
		t = f.Type()
	}
}

func (b *builder) compositeRefs(lit *ast.CompositeLit, refs refSet) {
	t := b.pkg.TypesInfo.TypeOf(lit)
	if t == nil {
		return
	}
	n := namedOf(t)
	if n == nil {
		return
	}
	st, ok := n.Underlying().(*types.Struct)
	if !ok {
		return
	}
	tk := b.typeKeyInModule(n)
	if tk == "" {
		return
	}
	refs.add(tk)
	refs.add(tk + "#fields")
	refs.add(tk + "#methods")
	for i, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok {
				refs.add(tk + "." + id.Name)
			}
		} else if i < st.NumFields() {
			refs.add(tk + "." + st.Field(i).Name())
		}
	}
}

func (b *builder) fieldKey(n *types.Named, field string) string {
	if tk := b.typeKeyInModule(n); tk != "" {
		return tk + "." + field
	}
	return ""
}

func (b *builder) typeKeyInModule(n *types.Named) string {
	if n.Obj().Pkg() == nil || !b.inModule(n.Obj().Pkg().Path()) {
		return ""
	}
	return typeKey(n)
}

// objKey returns the node key of a package-level object of the module, or
// "" for local objects, struct fields and objects outside the module.
func (b *builder) objKey(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil || !b.inModule(obj.Pkg().Path()) {
		return ""
	}
	path := obj.Pkg().Path()
	switch o := obj.(type) {
	case *types.Func:
		o = o.Origin()
		if recv := o.Type().(*types.Signature).Recv(); recv != nil {
			if n := namedOf(recv.Type()); n != nil {
				return typeKey(n) + "." + o.Name()
			}
			return ""
		}
		if o.Parent() != o.Pkg().Scope() {
			return ""
		}
		return path + "." + o.Name()
	case *types.Var:
		if o.IsField() || o.Origin().Parent() != o.Pkg().Scope() {
			return ""
		}
		return path + "." + o.Name()
	case *types.Const, *types.TypeName:
		if o.Parent() != o.Pkg().Scope() {
			return ""
		}
		return path + "." + o.Name()
	}
	return ""
}

func deref(t types.Type) types.Type {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return types.Unalias(p.Elem())
	}
	return t
}

func namedOf(t types.Type) *types.Named {
	if t == nil {
		return nil
	}
	if n, ok := deref(t).(*types.Named); ok {
		return n.Origin()
	}
	return nil
}

func typeKey(n *types.Named) string {
	o := n.Obj()
	if o.Pkg() == nil {
		return ""
	}
	return o.Pkg().Path() + "." + o.Name()
}

func canonicalFile(name string) string {
	dir, file := filepath.Split(name)
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(d, file)
	}
	return filepath.Clean(name)
}

// skipLines marks lines that are blank or contain only a // comment (but
// not a //go: directive), so that formatting and comment edits do not count
// as changes.
func skipLines(path string) []bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	skip := make([]bool, len(lines)+2)
	for i, l := range lines {
		l = strings.TrimSpace(l)
		directive := strings.HasPrefix(l, "//go:") || strings.HasPrefix(l, "//line ") || strings.HasPrefix(l, "// +build")
		skip[i+1] = l == "" || (strings.HasPrefix(l, "//") && !directive)
	}
	return skip
}
