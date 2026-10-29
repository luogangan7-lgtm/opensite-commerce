// Responsibility: ensure production wiring cannot select a settled test adapter.
// Dependencies: the actual production composition AST; CI's ordinary Go suite.
// Invariant: FromEnvironment always replaces supplied authority with unknown.
package bootstrap

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestMerchantDeletionProductionSettlementWiring(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "runtime.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		assignment, ok := n.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		selector, ok := assignment.Lhs[0].(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "PaymentDeletion" {
			return true
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			t.Fatal("MDEL04_PRODUCTION_SETTLEMENT_WIRING")
		}
		constructor, ok := call.Fun.(*ast.SelectorExpr)
		argument, nilArgument := call.Args[0].(*ast.Ident)
		if !ok || constructor.Sel.Name != "NewDeletionObligations" || !nilArgument || argument.Name != "nil" {
			t.Fatal("MDEL04_PRODUCTION_SETTLEMENT_WIRING")
		}
		found = true
		return true
	})
	if !found {
		t.Fatal("MDEL04_MISSING_PRODUCTION_GATE")
	}
}
