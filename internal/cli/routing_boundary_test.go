package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file pins one architectural claim: the gateway does not read
// routing.json. It is held by the import graph rather than by discipline, and
// the architecture relies on it, so it needs to be something that fails when it
// stops being true.
//
// WHAT THE GUARANTEE IS, EXACTLY
//
// It is "no gateway package can call the parser", NOT "the parser is absent
// from the daemon binary". LoadRouting IS linked into standpiped, because
// standpiped is one line over internal/cli and internal/cli holds the
// parser. TestRoutingParserIsLinkedIntoBothBinaries below asserts that on
// purpose. Do not "strengthen" this file into a claim that the code is absent
// from the daemon — that claim is false, and a test asserting it would either
// fail immediately or be made to pass by moving the parser somewhere worse.
//
// This is the same distinction the rename recorded for `standpipe gateway`:
// a verb that refuses is not the same thing as code that is not there.
//
// WHY THE IMPORT GRAPH AND NOT A SYMBOL
//
// The honest unit of this claim is reachability, not the presence or absence of
// a call. A gateway package that imports the parser's package has the ability to
// read routing.json whether or not today's code exercises it, and one added
// import is exactly how the guarantee would be lost with no other symptom.
//
// WHY THIS PARSES SOURCE INSTEAD OF SHELLING OUT TO `go list`
//
//   - It always runs. A test that needs the toolchain in $PATH would have to
//     skip when it is missing, and an architectural test that skips reads as
//     coverage while proving nothing.
//   - It is build-tag blind, which here is a feature. `go list -deps` answers
//     for one GOOS/GOARCH; internal/store already carries disk_unix.go and
//     disk_other.go. A prohibition should be conservative: an import that only
//     exists on Windows is still an import, and this scan sees it.
//   - It is fast and needs no module cache or network.
//
// The trade is that this sees the module's own packages only, which is all the
// claim is about, and that it trusts source over the compiler's resolution. The
// two were compared by hand when this test was written: `go list -deps` on both
// binaries agrees with the graph built here.

// modulePrefix returns the module root directory and the module path, found by
// walking up from the test's working directory to the go.mod that declares it.
// Nothing here is hardcoded, so a module rename does not silently blind the
// test — it either follows the rename or fails to find a root.
func modulePrefix(t *testing.T) (root, modPath string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		gomod := filepath.Join(dir, "go.mod")
		if b, err := os.ReadFile(gomod); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if rest, ok := strings.CutPrefix(line, "module "); ok {
					return dir, strings.TrimSpace(rest)
				}
			}
			t.Fatalf("%s has no module line", gomod)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %q", dir)
		}
		dir = parent
	}
}

// modPkg is one package of this module as read off its source.
type modPkg struct {
	importPath string
	name       string          // "main" for a composition root
	imports    map[string]bool // module-local imports only
	declares   map[string]bool // top-level func and type names
}

// scanModule builds the module's own import graph.
//
// _test.go files are excluded deliberately. The claim is about what the shipped
// gateway can do; a test file's import does not put routing.json inside the
// request path. If you want to widen this later, widen it knowingly.
func scanModule(t *testing.T) map[string]*modPkg {
	t.Helper()
	root, modPath := modulePrefix(t)
	pkgs := map[string]*modPkg{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if path != root && (base == "vendor" || base == "testdata" || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			t.Fatalf("rel %s: %v", path, rerr)
		}
		ip := modPath
		if rel != "." {
			ip = modPath + "/" + filepath.ToSlash(rel)
		}
		p := pkgs[ip]
		if p == nil {
			p = &modPkg{importPath: ip, name: f.Name.Name, imports: map[string]bool{}, declares: map[string]bool{}}
			pkgs[ip] = p
		}
		for _, spec := range f.Imports {
			target := strings.Trim(spec.Path.Value, `"`)
			if target == modPath || strings.HasPrefix(target, modPath+"/") {
				p.imports[target] = true
			}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					p.declares[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok {
						p.declares[ts.Name.Name] = true
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(pkgs) == 0 {
		t.Fatalf("scanned %s and found no packages; the scan is broken, not the boundary", root)
	}
	return pkgs
}

// routingParserPkg finds the package that can read routing.json, by looking for
// the declarations that do it rather than by naming internal/cli.
//
// Deriving it is the point. If a later slice moves LoadRouting into
// internal/config because "config parsing belongs in config", a test that
// hardcoded internal/cli would keep passing while every gateway package gained
// the ability the documents say it does not have. This follows the parser.
//
// If the anchors are renamed the test fails loudly rather than passing on an
// empty set — that failure is the signal to re-point it, not to delete it.
func routingParserPkg(t *testing.T, pkgs map[string]*modPkg) *modPkg {
	t.Helper()
	const anchorFunc = "LoadRouting"
	const anchorType = "RoutingFile"
	var found []*modPkg
	for _, p := range pkgs {
		if p.declares[anchorFunc] || p.declares[anchorType] {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("no package declares %s or %s; if the routing.json parser was renamed, re-point these anchors — do not delete this test", anchorFunc, anchorType)
	default:
		var names []string
		for _, p := range found {
			names = append(names, p.importPath)
		}
		sort.Strings(names)
		t.Fatalf("the routing.json parser is split across %v; the routing.json boundary needs one home", names)
	}
	return nil
}

// reachers returns every package that can reach target through module-local
// imports, transitively, including target itself.
func reachers(pkgs map[string]*modPkg, target string) map[string]bool {
	reaches := map[string]bool{target: true}
	for changed := true; changed; {
		changed = false
		for ip, p := range pkgs {
			if reaches[ip] {
				continue
			}
			for imp := range p.imports {
				if reaches[imp] {
					reaches[ip] = true
					changed = true
					break
				}
			}
		}
	}
	return reaches
}

// TestGatewayCannotReachRoutingParser is the pin.
//
// The set of packages under test is DERIVED: every package of this module that
// is not a `main` and is not the parser's own package. A hardcoded list of
// gateway packages would stop covering the next package somebody adds, which is
// this codebase's recurring bug class — a guard placed where the thing it guards
// against cannot reach it.
//
// `main` packages are exempt because a composition root is allowed to link the
// operator CLI; both binaries are required to be thin wrappers over it. Everything else is
// a library, and a library that can reach the parser can read routing.json from
// inside the request path.
func TestGatewayCannotReachRoutingParser(t *testing.T) {
	pkgs := scanModule(t)
	parserPkg := routingParserPkg(t, pkgs)
	reaches := reachers(pkgs, parserPkg.importPath)

	// Anti-vacuity. Not a coverage list — coverage is derived above. These are
	// the three gateway packages the boundary is about, and if the scan
	// did not even see them then a green result means the walk failed, not that
	// the boundary holds.
	_, modPath := modulePrefix(t)
	for _, suffix := range []string{"/internal/httpapi", "/internal/forge", "/internal/store"} {
		if pkgs[modPath+suffix] == nil {
			t.Fatalf("scan did not find %s; a pass would be meaningless", modPath+suffix)
		}
	}

	var violations []string
	for ip, p := range pkgs {
		if ip == parserPkg.importPath || p.name == "main" {
			continue
		}
		if reaches[ip] {
			violations = append(violations, ip)
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("these non-main packages can reach the routing.json parser in %s: %v\n"+
			"the gateway does not read routing.json — its consumer is the caller,\n"+
			"reading a local clone while the gateway is down. A gateway package that can\n"+
			"reach the parser makes the direct-call path depend on the thing that is down.\n"+
			"If this boundary is genuinely being moved, the documents change first, not this test.",
			parserPkg.importPath, violations)
	}
}

// TestRoutingParserIsLinkedIntoBothBinaries records the nuance in the same place
// as the prohibition, so the two are read together.
//
// standpiped DOES contain the routing.json parser: it is one line over
// internal/cli and internal/cli holds it. The guarantee in the test above is
// about ability, not about absence. This also proves the reachability walk can
// answer "yes" at all — without it, a bug that made reachers() return nothing
// would make the prohibition pass for the wrong reason.
func TestRoutingParserIsLinkedIntoBothBinaries(t *testing.T) {
	pkgs := scanModule(t)
	parserPkg := routingParserPkg(t, pkgs)
	reaches := reachers(pkgs, parserPkg.importPath)

	var mains []string
	for ip, p := range pkgs {
		if p.name == "main" {
			mains = append(mains, ip)
		}
	}
	sort.Strings(mains)
	if len(mains) < 2 {
		t.Fatalf("expected both binaries as main packages, found %v", mains)
	}
	for _, ip := range mains {
		if !reaches[ip] {
			t.Errorf("%s does not reach %s; both binaries must be one line over the operator CLI", ip, parserPkg.importPath)
		}
	}
}
