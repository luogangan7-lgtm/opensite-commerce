// Responsibility: type-check the adapter's exported API for raw connection capability.
// Dependencies: go/types with the source importer; callers: go test (default and pgx tags).
// Invariant: reviewed exports cannot yield raw or opaque connection capabilities.
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
	"github.com/jackc/pgx/v5/pgconn":  {"PgConn": true, "Pipeline": true},
	"database/sql/driver":             {"Connector": true, "Driver": true, "DriverContext": true, "Conn": true, "Stmt": true},
	"database/sql":                    {"DB": true, "Conn": true, "Stmt": true, "Tx": true},
}

type exposureKey struct {
	typeOf types.Type
	opaque bool
}

// exposure reports the first forbidden type reachable from t outside pkg.
// Foreign named types are opaque (pgx.Tx is the transaction-scoped contract);
// local types expand through public fields/methods. Opaque inputs (Scan(...any))
// are legal, but outputs/fields cannot box a connection. Track both directions
// so visiting an input first cannot hide the same type in an output.
func exposure(t types.Type, pkg *types.Package, seen map[exposureKey]bool, opaque bool) string {
	t = types.Unalias(t)
	key := exposureKey{t, opaque}
	if seen[key] {
		return ""
	}
	seen[key] = true
	if opaque {
		if it, ok := t.Underlying().(*types.Interface); ok && it.Empty() {
			return "opaque empty interface"
		}
	}
	switch t := t.(type) {
	case *types.Basic:
		if opaque && t.Kind() == types.UnsafePointer {
			return "unsafe.Pointer"
		}
	case *types.Named:
		if o := t.Obj(); o.Pkg() != nil {
			if rawTypes[o.Pkg().Path()][o.Name()] {
				return o.Pkg().Path() + "." + o.Name()
			}
			if opaque && o.Pkg().Path() == "reflect" && o.Name() == "Value" {
				return "reflect.Value"
			}
			if o.Pkg() != pkg {
				// pgx value types are inspected through their exported methods; other
				// foreign types (context, errors, River) carry no connection capability.
				if strings.HasPrefix(o.Pkg().Path(), "github.com/jackc/pgx/v5") {
					return foreignExposure(t, pkg, seen)
				}
				return ""
			}
		}
		if s := exposure(t.Underlying(), pkg, seen, opaque); s != "" {
			return s
		}
		for _, mt := range []types.Type{t, types.NewPointer(t)} {
			set := types.NewMethodSet(mt)
			for i := 0; i < set.Len(); i++ {
				if m := set.At(i).Obj(); m.Exported() {
					if s := exposure(m.Type(), pkg, seen, opaque); s != "" {
						return s
					}
				}
			}
		}
	case *types.Pointer:
		return exposure(t.Elem(), pkg, seen, opaque)
	case *types.Slice:
		return exposure(t.Elem(), pkg, seen, opaque)
	case *types.Array:
		return exposure(t.Elem(), pkg, seen, opaque)
	case *types.Chan:
		return exposure(t.Elem(), pkg, seen, opaque)
	case *types.Map:
		if s := exposure(t.Key(), pkg, seen, opaque); s != "" {
			return s
		}
		return exposure(t.Elem(), pkg, seen, opaque)
	case *types.Tuple:
		for i := 0; i < t.Len(); i++ {
			if s := exposure(t.At(i).Type(), pkg, seen, opaque); s != "" {
				return s
			}
		}
	case *types.Signature:
		if s := exposure(t.Params(), pkg, seen, false); s != "" {
			return s
		}
		return exposure(t.Results(), pkg, seen, opaque)
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if f := t.Field(i); f.Exported() || f.Embedded() {
				if s := exposure(f.Type(), pkg, seen, opaque); s != "" {
					return s
				}
			}
		}
	case *types.Interface:
		for i := 0; i < t.NumMethods(); i++ {
			if m := t.Method(i); m.Exported() {
				if s := exposure(m.Type(), pkg, seen, opaque); s != "" {
					return s
				}
			}
		}
	case *types.TypeParam:
		return exposure(t.Constraint(), pkg, seen, opaque)
	}
	return ""
}

// transactionPort models the one accepted contract: callbacks receive pgx.Tx and pgx.Rows,
// whose Conn() accessor is part of the transaction-scoped port. Any other path to a raw
// connection through pgx types fails. Retaining Conn() past commit remains a caller bug that
// only a wrapper type without Conn() could remove.
var transactionPort = map[string]bool{
	"github.com/jackc/pgx/v5.Tx.Conn":   true,
	"github.com/jackc/pgx/v5.Rows.Conn": true,
}

func foreignExposure(t *types.Named, pkg *types.Package, seen map[exposureKey]bool) string {
	var methods []*types.Func
	if it, ok := t.Underlying().(*types.Interface); ok {
		for i := 0; i < it.NumMethods(); i++ {
			methods = append(methods, it.Method(i))
		}
	} else {
		// Function types and structs carry connection pointers in their signature or fields.
		if s := exposure(t.Underlying(), pkg, seen, false); s != "" {
			return t.Obj().Name() + " -> " + s
		}
		for i := 0; i < t.NumMethods(); i++ {
			methods = append(methods, t.Method(i))
		}
	}
	for _, m := range methods {
		key := t.Obj().Pkg().Path() + "." + t.Obj().Name() + "." + m.Name()
		if !m.Exported() || transactionPort[key] {
			continue
		}
		// Existing transaction result ports (Rows.Values, Row.Scan) carry any;
		// audit their raw accessors, not foreign value serialization contracts.
		if s := exposure(m.Type(), pkg, seen, false); s != "" {
			return key + " -> " + s
		}
	}
	return ""
}

func exposures(pkg *types.Package) []string {
	var out []string
	for name, obj := range exportedSurface(pkg) {
		if _, isConst := obj.(*types.Const); isConst {
			continue
		}
		if s := exposure(obj.Type(), pkg, map[exposureKey]bool{}, true); s != "" {
			out = append(out, name+" exposes "+s)
		}
	}
	sort.Strings(out)
	return out
}

var sourceImporter = importer.ForCompiler(token.NewFileSet(), "source", nil)

var reviewedPGXExports = []string{
	"type:Runner",
	"func:Open",
	"func:OpenPublic",
	"method:Runner.InMaintenance",
	"method:Runner.InPublicRead",
	"method:Runner.InTenant",
	"method:Runner.InTenantSnapshot",
	"method:Runner.RunAuthorizedTx",
}

func exportedSurface(pkg *types.Package) map[string]types.Object {
	out := map[string]types.Object{}
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}
		kind := ""
		switch obj.(type) {
		case *types.TypeName:
			kind = "type"
		case *types.Func:
			kind = "func"
		case *types.Var:
			kind = "var"
		case *types.Const:
			kind = "const"
		}
		out[kind+":"+name] = obj
		if kind != "type" {
			continue
		}
		typ := types.Unalias(obj.Type())
		// Pointer and value sets cover pointer receivers, interfaces, and promoted
		// methods on private embedded types. Use the exported carrier's name.
		for _, mt := range []types.Type{typ, types.NewPointer(typ)} {
			set := types.NewMethodSet(mt)
			for i := 0; i < set.Len(); i++ {
				if method := set.At(i).Obj(); method.Exported() {
					out["method:"+name+"."+method.Name()] = method
				}
			}
		}
		seenFields := map[types.Type]bool{}
		var fields func(types.Type)
		fields = func(t types.Type) {
			t = types.Unalias(t)
			if ptr, ok := t.(*types.Pointer); ok {
				t = types.Unalias(ptr.Elem())
			}
			if seenFields[t] {
				return
			}
			seenFields[t] = true
			st, ok := t.Underlying().(*types.Struct)
			if !ok {
				return
			}
			for i := 0; i < st.NumFields(); i++ {
				if field := st.Field(i); field.Exported() {
					// Resolve from the public carrier: omit shadowed or ambiguous
					// fields, include visible fields promoted from private embeddings.
					obj, _, _ := types.LookupFieldOrMethod(typ, true, nil, field.Name())
					if visible, ok := obj.(*types.Var); ok && visible.IsField() {
						out["field:"+name+"."+visible.Name()] = visible
					}
				}
				if field := st.Field(i); field.Embedded() {
					fields(field.Type())
				}
			}
		}
		fields(typ)
	}
	return out
}

func unreviewedExports(pkg *types.Package, reviewed []string) []string {
	actual := exportedSurface(pkg)
	var out []string
	for _, name := range reviewed {
		if actual[name] == nil {
			out = append(out, "missing "+name)
		}
		delete(actual, name)
	}
	for name := range actual {
		out = append(out, "unreviewed "+name)
	}
	sort.Strings(out)
	return out
}

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
		t.Fatalf("adapter API hands out raw or opaque connection capability: %s", strings.Join(got, "; "))
	}
	reviewed := append([]string(nil), reviewedAdapterExports...)
	if pgxTagActive {
		reviewed = append(reviewed, reviewedPGXExports...)
	}
	if got := unreviewedExports(pkg, reviewed); len(got) != 0 {
		t.Fatalf("adapter exports require explicit review: %s", strings.Join(got, "; "))
	}
}

func TestExportSurfaceRequiresExplicitReview(t *testing.T) {
	const base = "package fixture\ntype Runner struct{}\nfunc (*Runner) Close() {}\n"
	reviewed := []string{"type:Runner", "method:Runner.Close"}
	cases := []struct {
		name, source, want string
	}{
		{"unchanged", base, ""},
		{"new function", base + "func Added() bool { return true }\n", "unreviewed func:Added"},
		{"new method", base + "func (Runner) Added() bool { return true }\n", "unreviewed method:Runner.Added"},
		{"new field", strings.Replace(base, "struct{}", "struct{ Added bool }", 1), "unreviewed field:Runner.Added"},
		{"new variable", base + "var Added bool\n", "unreviewed var:Added"},
		{"new constant", base + "const Added = true\n", "unreviewed const:Added"},
		{"new type", base + "type Added bool\n", "unreviewed type:Added"},
		{"promoted method", strings.Replace(base, "struct{}", "struct{ hidden }", 1) + "type hidden struct{}\nfunc (*hidden) Added() {}\n", "unreviewed method:Runner.Added"},
		{"promoted field", strings.Replace(base, "struct{}", "struct{ *hidden }", 1) + "type hidden struct{ Added bool }\n", "unreviewed field:Runner.Added"},
		{"removed export", "package fixture\ntype Runner struct{}\n", "missing method:Runner.Close"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(unreviewedExports(check(t, map[string]string{"a.go": tc.source}), reviewed), "; ")
			if got != tc.want {
				t.Fatalf("review gate got %q, want %q", got, tc.want)
			}
		})
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
		"grouped type factory":       {"a.go": header + "type (Provider interface { Open() (pool *pgxpool.Pool, err error) })\n"},
		"grouped var factory":        {"a.go": header + "var (RawFactory func() (pool *pgxpool.Pool, err error))\n"},
		"cross file alias":           {"a.go": header + "func rawPool() *pgxpool.Pool { return nil }\n", "b.go": "package fixture\nvar layer = rawPool\nvar RawPool = layer\n"},
		"struct field factory":       {"a.go": header + "type Provider struct { Open func() (pool *pgxpool.Pool, err error) }\n"},
		"private variable alias":     {"a.go": header + "var pool *pgxpool.Pool\nvar Pool = pool\n"},
		"method value":               {"a.go": header + "type holder struct{}\nfunc (holder) open() *pgxpool.Pool { return nil }\nvar Open = holder.open\n"},
		"composite literal field":    {"a.go": header + "var Open = struct{ pool *pgxpool.Pool }{}.pool\n"},
		"anonymous interface field":  {"a.go": header + "type Provider struct { Open interface { Get() *pgxpool.Pool } }\n"},
		"sql statement":              {"a.go": header + "func Prepared() *sql.Stmt { return nil }\n"},
		"sql transaction":            {"a.go": header + "func BeginRaw() *sql.Tx { return nil }\n"},
		"pgconn pipeline":            {"a.go": "package fixture\nimport \"github.com/jackc/pgx/v5/pgconn\"\nvar _ pgconn.CommandTag\nfunc RawPipeline() *pgconn.Pipeline { return nil }\n"},
		"driver connector":           {"a.go": "package fixture\nimport \"database/sql/driver\"\nfunc C() driver.Connector { return nil }\n"},
		"foreign callback type":      {"a.go": "package fixture\nimport \"github.com/jackc/pgx/v5/pgconn\"\nfunc Install(h pgconn.NoticeHandler) {}\n"},
		"exported method":            {"a.go": header + "type Runner struct{ pool *pgxpool.Pool }\nfunc (r Runner) Raw() *pgxpool.Pool { return r.pool }\n"},
		"unexported carrier":         {"a.go": header + "type runner struct{ pool *pgxpool.Pool }\nfunc (r runner) Pool() *pgxpool.Pool { return r.pool }\nfunc New() runner { return runner{} }\n"},
		"pgx connection value":       {"a.go": header + "var Open = pgx.Connect\n"},
		"opaque function result":     {"a.go": header + "func Raw() any { return (*pgxpool.Pool)(nil) }\n"},
		"opaque method result":       {"a.go": header + "type Runner struct{ pool *pgxpool.Pool }\nfunc (r Runner) Raw() any { return r.pool }\n"},
		"promoted opaque method":     {"a.go": header + "type hidden struct{}\nfunc (*hidden) Raw() any { return nil }\ntype Runner struct{ *hidden }\n"},
		"empty interface result":     {"a.go": header + "func Raw() interface{} { return nil }\n"},
		"empty interface alias":      {"a.go": header + "type empty = interface{}\ntype boxed = empty\nfunc Raw() boxed { return nil }\n"},
		"named empty interface":      {"a.go": header + "type Box interface{}\nfunc Raw() Box { return nil }\n"},
		"opaque field":               {"a.go": header + "type Runner struct { Raw any }\n"},
		"opaque container result":    {"a.go": header + "func Raw() map[string][]any { return nil }\n"},
		"opaque channel result":      {"a.go": header + "func Raw() <-chan any { return nil }\n"},
		"opaque callback result":     {"a.go": header + "func Raw() func() any { return nil }\n"},
		"opaque result after input":  {"a.go": header + "func Raw(input any) any { return input }\n"},
		"unsafe pointer result":      {"a.go": "package fixture\nimport \"unsafe\"\nfunc Raw() unsafe.Pointer { return nil }\n"},
		"unsafe pointer alias":       {"a.go": "package fixture\nimport \"unsafe\"\ntype pointer = unsafe.Pointer\nfunc Raw() pointer { return nil }\n"},
		"reflect value result":       {"a.go": "package fixture\nimport \"reflect\"\nfunc Raw() reflect.Value { return reflect.Value{} }\n"},
		"reflect value field":        {"a.go": "package fixture\nimport \"reflect\"\ntype Runner struct { Raw reflect.Value }\n"},
		"reflect value alias":        {"a.go": "package fixture\nimport \"reflect\"\ntype value = reflect.Value\nfunc Raw() value { return value{} }\n"},
		"foreign alias opaque field": {"a.go": "package fixture\nimport \"reflect\"\ntype Method = reflect.Method\n"},
		"unsafe pointer field":       {"a.go": "package fixture\nimport \"unsafe\"\ntype Runner struct { Raw unsafe.Pointer }\n"},
		"generic opaque result":      {"a.go": header + "func Raw[T any](value T) T { return value }\n"},
		"raw pool alias":             {"a.go": header + "type raw = pgxpool.Pool\nfunc Raw() *raw { return nil }\n"},
		"raw pooled connection":      {"a.go": header + "func Raw() *pgxpool.Conn { return nil }\n"},
		"raw connection result":      {"a.go": header + "func Raw() *pgx.Conn { return nil }\n"},
	}
	t.Run("red pgx.Tx.Conn without the transaction-port allowance", func(t *testing.T) {
		saved := transactionPort
		transactionPort = map[string]bool{}
		defer func() { transactionPort = saved }()
		if len(exposures(check(t, map[string]string{"a.go": header + "var T pgx.Tx\n"}))) == 0 {
			t.Fatal("foreign method set was not inspected")
		}
	})
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
		"opaque input only":        "func Scan(dest ...any) error { return nil }\n",
		"private opaque field":     "type Runner struct{ hidden any }\n",
		"context input and result": "func Echo(ctx interface{ Done() <-chan struct{} }) interface{ Done() <-chan struct{} } { return ctx }\n",
		"transaction result port":  "func Begin() (pgx.Tx, error) { return nil, nil }\nfunc Query() pgx.Rows { return nil }\n",
	}
	for name, body := range green {
		t.Run("green "+name, func(t *testing.T) {
			if got := exposures(check(t, map[string]string{"a.go": header + body})); len(got) != 0 {
				t.Fatalf("false positive: %v", got)
			}
		})
	}
}
