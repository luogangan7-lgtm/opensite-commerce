// Command privacyopaque verifies the closed HTTP key admission boundary for
// retained opaque references. Depends only on Go AST; privacy_registry.py calls
// it in CI. It checks control flow and imported call identity, not comments.
// It is deliberately not a general Go/SQL interprocedural taint analyser:
// provider identities and existing generated SQL remain separate authorities.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const shared = "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"

func source(node ast.Node) string {
	var out bytes.Buffer
	_ = printer.Fprint(&out, token.NewFileSet(), node)
	return out.String()
}

func imported(file *ast.File, alias, path string) bool {
	for _, item := range file.Imports {
		value, _ := strconv.Unquote(item.Path.Value)
		name := filepath.Base(value)
		if item.Name != nil {
			name = item.Name.Name
		}
		if name == alias && value == path {
			return true
		}
	}
	return false
}

func sharedCall(file *ast.File, node ast.Expr) bool {
	call, ok := node.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || source(call.Args[0]) != "r.Header" {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ValidateKey" {
		return false
	}
	name, ok := sel.X.(*ast.Ident)
	return ok && name.Obj == nil && imported(file, name.Name, shared)
}

func terminal(block *ast.BlockStmt) bool {
	if block == nil || len(block.List) == 0 {
		return false
	}
	_, ok := block.List[len(block.List)-1].(*ast.ReturnStmt)
	return ok
}

func sharedGuard(file *ast.File, stmt ast.Stmt) bool {
	outer, ok := stmt.(*ast.IfStmt)
	if !ok || outer.Init != nil || outer.Else != nil || source(outer.Cond) != "requiresKey" || len(outer.Body.List) != 1 {
		return false
	}
	inner, ok := outer.Body.List[0].(*ast.IfStmt)
	if !ok || inner.Else != nil || !terminal(inner.Body) {
		return false
	}
	assign, ok := inner.Init.(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 || !sharedCall(file, assign.Rhs[0]) {
		return false
	}
	errName, ok := assign.Lhs[1].(*ast.Ident)
	if !ok || errName.Name == "_" || source(inner.Cond) != errName.Name+" != nil" {
		return false
	}
	// The rejecting branch must be straight-line: an early success return,
	// nested condition, panic recovery or deferred continuation is not proof.
	for _, statement := range inner.Body.List[:len(inner.Body.List)-1] {
		if _, ok := statement.(*ast.ExprStmt); !ok {
			return false
		}
	}
	return true
}

func binding(fn *ast.FuncDecl, name, expression string) int {
	for index, stmt := range fn.Body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			continue
		}
		if assign.Tok == token.DEFINE && len(assign.Lhs) > 0 && source(assign.Lhs[0]) == name && source(assign.Rhs[0]) == expression {
			return index
		}
	}
	return -1
}

func checkedNext(fn *ast.FuncDecl, index int, condition string) bool {
	if index < 0 || index+1 >= len(fn.Body.List) {
		return false
	}
	check, ok := fn.Body.List[index+1].(*ast.IfStmt)
	return ok && check.Init == nil && check.Else == nil && source(check.Cond) == condition && terminal(check.Body)
}

func contractHelper(fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) < 8 || len(fn.Body.List) > 9 || source(fn.Type) != "func(spec RouteSpec) (bool, error)" {
		return false
	}
	if !checkedNext(fn, binding(fn, "doc", "GetSwagger()"), "err != nil") ||
		!checkedNext(fn, binding(fn, "item", "doc.Paths.Value(spec.Pattern)"), "item == nil") ||
		!checkedNext(fn, binding(fn, "op", "item.GetOperation(spec.Method)"), "op == nil") {
		return false
	}
	if binding(fn, "doc", "GetSwagger()") != 0 || binding(fn, "item", "doc.Paths.Value(spec.Pattern)") != 2 || binding(fn, "op", "item.GetOperation(spec.Method)") != 4 {
		return false
	}
	errorCheck := fn.Body.List[1].(*ast.IfStmt)
	errorReturn := errorCheck.Body.List[len(errorCheck.Body.List)-1].(*ast.ReturnStmt)
	if len(errorReturn.Results) != 2 || source(errorReturn.Results[0]) != "false" || source(errorReturn.Results[1]) == "nil" {
		return false
	}
	final, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ReturnStmt)
	if !ok || len(final.Results) != 2 || source(final.Results[0]) != "false" || source(final.Results[1]) != "nil" {
		return false
	}
	// Follow both inherited path and operation parameter collections to their
	// range loops. Reject hard-coded operation lists and a forged requiresKey.
	loop, ok := fn.Body.List[len(fn.Body.List)-2].(*ast.RangeStmt)
	if !ok || source(loop.Key) != "_" || loop.Tok != token.DEFINE {
		return false
	}
	expr := loop.X
	if len(fn.Body.List) == 9 {
		assign, ok := fn.Body.List[6].(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || source(assign.Lhs[0]) != "parameters" || source(expr) != "parameters" {
			return false
		}
		expr = assign.Rhs[0]
	}
	collection := source(expr)
	if collection != "[]openapi3.Parameters{item.Parameters, op.Parameters}" && collection != "append(append(Parameters{}, item.Parameters...), op.Parameters...)" {
		return false
	}
	body := loop.Body
	if collection == "[]openapi3.Parameters{item.Parameters, op.Parameters}" {
		if source(loop.Value) != "parameters" || len(body.List) != 1 {
			return false
		}
		nested, ok := body.List[0].(*ast.RangeStmt)
		if !ok || source(nested.X) != "parameters" || source(nested.Key) != "_" || source(nested.Value) != "parameter" || nested.Tok != token.DEFINE {
			return false
		}
		body = nested.Body
	} else if source(loop.Value) != "parameter" {
		return false
	}
	if len(body.List) != 1 {
		return false
	}
	check, ok := body.List[0].(*ast.IfStmt)
	if !ok || check.Init != nil || check.Else != nil || source(check.Cond) != `parameter.Value != nil && parameter.Value.In == "header" && strings.EqualFold(parameter.Value.Name, "Idempotency-Key")` || len(check.Body.List) != 1 {
		return false
	}
	result, ok := check.Body.List[0].(*ast.ReturnStmt)
	return ok && len(result.Results) == 2 && source(result.Results[0]) == "true" && source(result.Results[1]) == "nil"
}

func routerBoundary(file *ast.File) bool {
	var register, helper *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Name.Name == "Register" && fn.Recv != nil {
				register = fn
			}
			if fn.Name.Name == "requiresIdempotencyKey" && fn.Recv == nil {
				helper = fn
			}
		}
	}
	if register == nil || helper == nil || !contractHelper(helper) || !imported(file, "strings", "strings") ||
		!checkedNext(register, binding(register, "requiresKey", "requiresIdempotencyKey(spec)"), "err != nil") {
		return false
	}
	// The policy bit must not be overwritten or shadowed after lookup. A valid
	// guard on a constant-false bit proves no admission at all.
	assignments := 0
	invalidBit := false
	ast.Inspect(register.Body, func(node ast.Node) bool {
		switch item := node.(type) {
		case *ast.AssignStmt:
			for _, lhs := range item.Lhs {
				if source(lhs) == "requiresKey" {
					assignments++
					if len(item.Lhs) != 2 || source(item.Lhs[1]) != "err" || item.Tok != token.DEFINE || len(item.Rhs) != 1 || source(item.Rhs[0]) != "requiresIdempotencyKey(spec)" {
						invalidBit = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, name := range item.Names {
				invalidBit = invalidBit || name.Name == "requiresKey"
			}
		}
		return true
	})
	if assignments != 1 || invalidBit {
		return false
	}
	// Only the closure actually passed to mux.Handle counts. An unreachable
	// sibling closure with a valid guard cannot attest another handler.
	closures, guarded := 0, 0
	ast.Inspect(register.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || source(call.Fun) != "s.mux.Handle" || len(call.Args) != 2 {
			return true
		}
		wrap, ok := call.Args[1].(*ast.CallExpr)
		if !ok || source(wrap.Fun) != "http.HandlerFunc" || len(wrap.Args) != 1 || !imported(file, "http", "net/http") {
			return true
		}
		closure, ok := wrap.Args[0].(*ast.FuncLit)
		if !ok {
			return true
		}
		closures++
		if len(closure.Body.List) > 0 && sharedGuard(file, closure.Body.List[0]) {
			guarded++
		}
		return true
	})
	return closures == 1 && guarded == 1
}

var writeSQL = regexp.MustCompile(`(?is)\b(?:INSERT\s+INTO|UPDATE)\s+(?:app\.)?([a-z_][a-z0-9_]*)\b`)

func staticString(expr ast.Expr) (string, bool) {
	switch item := expr.(type) {
	case *ast.BasicLit:
		if item.Kind == token.STRING {
			value, err := strconv.Unquote(item.Value)
			return value, err == nil
		}
	case *ast.ParenExpr:
		return staticString(item.X)
	case *ast.BinaryExpr:
		if item.Op == token.ADD {
			left, leftOK := staticString(item.X)
			right, rightOK := staticString(item.Y)
			return left + right, leftOK && rightOK
		}
	}
	return "", false
}

func inventory(root string, columns map[string][]string) error {
	fset := token.NewFileSet()
	routerOK := false
	returnErr := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("invalid Go source %s", rel)
		}
		if rel == "internal/platform/httpapi/router.go" {
			routerOK = routerBoundary(file)
		}
		if strings.HasPrefix(rel, "internal/platform/httpapi/generated/") || strings.HasPrefix(rel, "internal/db/") || rel == "internal/platform/idempotency/request.go" {
			return nil
		}
		var failure error
		ast.Inspect(file, func(node ast.Node) bool {
			if failure != nil {
				return false
			}
			switch item := node.(type) {
			case *ast.CallExpr:
				selector, ok := item.Fun.(*ast.SelectorExpr)
				if !ok || len(item.Args) == 0 {
					return true
				}
				method := selector.Sel.Name
				// Existing adapter and test-role utilities own dynamic SQL as a
				// separate boundary. Application writers may not hide a new sink
				// behind a runtime-built SQL string or a caller-supplied query.
				if len(item.Args) > 1 && (method == "Exec" || method == "Query" || method == "QueryRow" || method == "ExecContext" || method == "QueryContext" || method == "QueryRowContext") &&
					!strings.HasPrefix(rel, "internal/tenanttx/pgxadapter/") && !strings.HasPrefix(rel, "internal/platform/pgxtest/") {
					value, constant := staticString(item.Args[1])
					if !constant {
						failure = fmt.Errorf("unreviewed dynamic SQL writer %s:%d", rel, fset.Position(item.Pos()).Line)
						return false
					}
					for _, match := range writeSQL.FindAllStringSubmatch(value, -1) {
						if len(columns[strings.ToLower(match[1])]) > 0 {
							failure = fmt.Errorf("unreviewed direct SQL writer %s:%d table %s", rel, fset.Position(item.Pos()).Line, match[1])
							return false
						}
					}
				}
				arg, ok := item.Args[0].(*ast.BasicLit)
				if !ok || arg.Kind != token.STRING {
					return true
				}
				name, _ := strconv.Unquote(arg.Value)
				if strings.EqualFold(name, "Idempotency-Key") && (method == "Get" || method == "Values" || method == "Set" || method == "Add" || method == "Del") && rel != "internal/modules/payments/stripe.go" {
					failure = fmt.Errorf("unchecked raw header ingress or mutation %s:%d", rel, fset.Position(item.Pos()).Line)
				}
			case *ast.IndexExpr:
				arg, ok := item.Index.(*ast.BasicLit)
				if ok && arg.Kind == token.STRING {
					name, _ := strconv.Unquote(arg.Value)
					if strings.EqualFold(name, "Idempotency-Key") {
						failure = fmt.Errorf("unchecked raw header index %s:%d", rel, fset.Position(item.Pos()).Line)
					}
				}
			case *ast.BasicLit:
				if item.Kind != token.STRING {
					return true
				}
				value, _ := strconv.Unquote(item.Value)
				for _, match := range writeSQL.FindAllStringSubmatch(value, -1) {
					if len(columns[strings.ToLower(match[1])]) > 0 {
						failure = fmt.Errorf("unreviewed direct SQL writer %s:%d table %s", rel, fset.Position(item.Pos()).Line, match[1])
					}
				}
			}
			return true
		})
		return failure
	})
	if returnErr != nil {
		return returnErr
	}
	if !routerOK {
		return fmt.Errorf("Router.Register must reject shared ValidateKey errors before any handler effect, selected by the OAS contract")
	}
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "MERCHANT_OPAQUE_PROVENANCE: root and opaque columns required")
		os.Exit(1)
	}
	columns := map[string][]string{}
	for _, key := range strings.Split(os.Args[2], ",") {
		parts := strings.Split(key, ".")
		if len(parts) == 2 {
			columns[parts[0]] = append(columns[parts[0]], parts[1])
		}
	}
	if err := inventory(os.Args[1], columns); err != nil {
		fmt.Fprintln(os.Stderr, "MERCHANT_OPAQUE_PROVENANCE: "+err.Error())
		os.Exit(1)
	}
}
