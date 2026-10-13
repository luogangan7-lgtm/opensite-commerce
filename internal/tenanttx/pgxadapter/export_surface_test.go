// Responsibility: type-check the adapter's exported API for raw connection capability.
// Dependencies: go/types with the source importer; callers: go test (default and pgx tags).
// Invariant: no exported object, field, method or signature can yield a pool or connection.
package pgxadapter_test

import (
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var rawTypes = map[string]map[string]bool{
	"github.com/jackc/pgx/v5/pgxpool": {"Pool": true, "Conn": true},
	"github.com/jackc/pgx/v5":         {"Conn": true},
	"github.com/jackc/pgx/v5/pgconn":  {"PgConn": true},
	"database/sql":                    {"DB": true, "Conn": true, "Stmt": true, "Tx": true},
}

// exposure reports the first raw type reachable from t by a caller outside pkg.
// Foreign named types are opaque (pgx.Tx is the transaction-scoped contract);
// types declared in pkg are expanded through fields and exported methods.
func exposure(t types.Type, pkg *types.Package, seen map[types.Type]bool) string {
	t = types.Unalias(t)
	if seen[t] {
		return ""
	}
	seen[t] = true
	switch t := t.(type) {
	case *types.Named:
		if o := t.Obj(); o.Pkg() != nil {
			if rawTypes[o.Pkg().Path()][o.Name()] {
				return o.Pkg().Path() + "." + o.Name()
			}
			if o.Pkg() != pkg {
				return ""
			}
		}
		if s := exposure(t.Underlying(), pkg, seen); s != "" {
			return s
		}
		for i := 0; i < t.NumMethods(); i++ {
			if m := t.Method(i); m.Exported() {
				if s := exposure(m.Type(), pkg, seen); s != "" {
					return s
				}
			}
		}
	case *types.Pointer:
		return exposure(t.Elem(), pkg, seen)
	case *types.Slice:
		return exposure(t.Elem(), pkg, seen)
	case *types.Array:
		return exposure(t.Elem(), pkg, seen)
	case *types.Chan:
		return exposure(t.Elem(), pkg, seen)
	case *types.Map:
		if s := exposure(t.Key(), pkg, seen); s != "" {
			return s
		}
		return exposure(t.Elem(), pkg, seen)
	case *types.Tuple:
		for i := 0; i < t.Len(); i++ {
			if s := exposure(t.At(i).Type(), pkg, seen); s != "" {
				return s
			}
		}
	case *types.Signature:
		if s := exposure(t.Params(), pkg, seen); s != "" {
			return s
		}
		return exposure(t.Results(), pkg, seen)
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if f := t.Field(i); f.Exported() || f.Embedded() {
				if s := exposure(f.Type(), pkg, seen); s != "" {
					return s
				}
			}
		}
	case *types.Interface:
		for i := 0; i < t.NumMethods(); i++ {
			if m := t.Method(i); m.Exported() {
				if s := exposure(m.Type(), pkg, seen); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func exposures(pkg *types.Package) []string {
	var out []string
	seen := map[types.Type]bool{}
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}
		if _, isConst := obj.(*types.Const); isConst {
			continue
		}
		if s := exposure(obj.Type(), pkg, seen); s != "" {
			out = append(out, name+" exposes "+s)
		}
	}
	sort.Strings(out)
	return out
}

var sourceImporter = importer.ForCompiler(token.NewFileSet(), "source", nil)

func check(t *testing.T, sources map[string]string) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, sources[name], 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: sourceImporter, Error: func(err error) { t.Error(err) }}
	pkg, _ := conf.Check("fixture", fset, files, nil)
	return pkg
}

func TestExportedAPIExposesNoRawConnectionCapability(t *testing.T) {
	ctx := build.Default
	if pgxTagActive {
		ctx.BuildTags = append(ctx.BuildTags, "pgx")
	}
	bp, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: sourceImporter, Error: func(err error) { t.Error(err) }}
	pkg, _ := conf.Check("github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter", fset, files, nil)
	if got := exposures(pkg); len(got) != 0 {
		t.Fatalf("adapter API hands out raw connection capability: %s", strings.Join(got, "; "))
	}
}

const header = `package fixture
import (
	"database/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)
var _ sql.NullString
var _ pgx.Tx
var _ pgxpool.Pool
`

func TestExportSurfaceDetectorRedAndGreen(t *testing.T) {
	red := map[string]map[string]string{
		"grouped type factory":      {"a.go": header + "type (Provider interface { Open() (pool *pgxpool.Pool, err error) })\n"},
		"grouped var factory":       {"a.go": header + "var (RawFactory func() (pool *pgxpool.Pool, err error))\n"},
		"cross file alias":          {"a.go": header + "func rawPool() *pgxpool.Pool { return nil }\n", "b.go": "package fixture\nvar layer = rawPool\nvar RawPool = layer\n"},
		"struct field factory":      {"a.go": header + "type Provider struct { Open func() (pool *pgxpool.Pool, err error) }\n"},
		"private variable alias":    {"a.go": header + "var pool *pgxpool.Pool\nvar Pool = pool\n"},
		"method value":              {"a.go": header + "type holder struct{}\nfunc (holder) open() *pgxpool.Pool { return nil }\nvar Open = holder.open\n"},
		"composite literal field":   {"a.go": header + "var Open = struct{ pool *pgxpool.Pool }{}.pool\n"},
		"anonymous interface field": {"a.go": header + "type Provider struct { Open interface { Get() *pgxpool.Pool } }\n"},
		"sql statement":             {"a.go": header + "func Prepared() *sql.Stmt { return nil }\n"},
		"sql transaction":           {"a.go": header + "func BeginRaw() *sql.Tx { return nil }\n"},
		"exported method":           {"a.go": header + "type Runner struct{ pool *pgxpool.Pool }\nfunc (r Runner) Raw() *pgxpool.Pool { return r.pool }\n"},
		"unexported carrier":        {"a.go": header + "type runner struct{ pool *pgxpool.Pool }\nfunc (r runner) Pool() *pgxpool.Pool { return r.pool }\nfunc New() runner { return runner{} }\n"},
		"pgx connection value":      {"a.go": header + "var Open = pgx.Connect\n"},
	}
	for name, sources := range red {
		t.Run("red "+name, func(t *testing.T) {
			if len(exposures(check(t, sources))) == 0 {
				t.Fatal("raw capability reached the exported API undetected")
			}
		})
	}
	green := map[string]string{
		"runner with private pool": "type Runner struct{ pool *pgxpool.Pool }\nfunc (r Runner) InTenant(fn func(pgx.Tx) error) error { return fn(nil) }\n",
		"bool derived from pool":   "var pool *pgxpool.Pool\nvar PoolConfigured = pool != nil\n",
		"private grouped factory":  "var (hidden func() *pgxpool.Pool)\n",
		"local variable named DB":  "func configure() { var DB *sql.DB; _ = DB }\n",
		"multi name initializers":  "func open() *pgxpool.Pool { return nil }\nvar Safe, hidden = 0, open\n",
		"shadowed alias name":      "type hidden = pgxpool.Pool\nfunc Safe(hidden int) {}\n",
		"tracer interface":         "var _ pgx.ConnectTracer = nil\n",
		"inert mapping types":      "var S sql.NullString\n",
	}
	for name, body := range green {
		t.Run("green "+name, func(t *testing.T) {
			if got := exposures(check(t, map[string]string{"a.go": header + body})); len(got) != 0 {
				t.Fatalf("false positive: %v", got)
			}
		})
	}
}
