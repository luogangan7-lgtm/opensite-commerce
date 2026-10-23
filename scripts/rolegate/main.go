// Command rolegate rejects cluster-role SQL written directly in Go tests.
// Responsibilities: parse every *_test.go outside the role-fixture helper package,
// fold string literals and named string constants, and fail on CREATE/ALTER/DROP
// ROLE, role GRANT/REVOKE, and pg_auth_members writes.
// Dependencies: Go standard library only (go/ast, go/parser, go/token, strconv).
// Called by: tests/test_pg_role_fixture.py (offline lane).
// Invariants: comments and testdata directories are ignored; the allowlist keeps
// the common advisory-lock helper's writes legal; exit 1 on findings.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The helper package owns the common advisory lock, so only it may write roles.
const defaultAllow = "internal/platform/pgxtest/"

var (
	createRole = regexp.MustCompile(`\bCREATE ROLE\b`)
	alterRole  = regexp.MustCompile(`\bALTER ROLE\b`)
	dropRole   = regexp.MustCompile(`\bDROP ROLE\b`)
	grantWord  = regexp.MustCompile(`\bGRANT\b`)
	revokeWord = regexp.MustCompile(`\bREVOKE\b`)
	onWord     = regexp.MustCompile(`\bON\b`)
	toWord     = regexp.MustCompile(`\bTO\b`)
	fromWord   = regexp.MustCompile(`\bFROM\b`)
	spaceRun   = regexp.MustCompile(`\s+`)
	// Object privileges contain ON; only cluster-global role memberships are banned.
	// The negative ON check mirrors the retired regex: any ON in the statement skips it.
	authMembers = regexp.MustCompile(`\b(?:INSERT INTO|UPDATE|DELETE FROM) (?:PG_CATALOG\.)?PG_AUTH_MEMBERS\b`)
)

type parsedFile struct {
	dir    string
	pkg    string
	file   *ast.File
	isTest bool
}

type constDef struct {
	name string
	expr ast.Expr
}

type finding struct {
	file  string
	line  int
	col   int
	value string
}

func main() {
	root := flag.String("root", ".", "directory to scan")
	allow := flag.String("allow", defaultAllow, "comma-separated path prefixes that may write roles")
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: rolegate: %v\n", err)
		os.Exit(2)
	}
	allows := normalizeAllows(*allow)

	fset := token.NewFileSet()
	var files []parsedFile
	var parseErrs []string
	walkErr := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(absRoot, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p == absRoot {
				return nil
			}
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			if isAllowed(rel+"/", allows) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		isTest := strings.HasSuffix(d.Name(), "_test.go")
		if isTest && isAllowed(rel, allows) {
			return nil
		}
		source, readErr := os.ReadFile(p)
		if readErr != nil {
			parseErrs = append(parseErrs, fmt.Sprintf("%s: %v", rel, readErr))
			return nil
		}
		f, parseErr := parser.ParseFile(fset, rel, source, parser.SkipObjectResolution)
		if parseErr != nil {
			parseErrs = append(parseErrs, parseErr.Error())
			if f == nil {
				return nil
			}
		}
		files = append(files, parsedFile{
			dir:    path.Dir(rel),
			pkg:    f.Name.Name,
			file:   f,
			isTest: isTest,
		})
		return nil
	})
	if walkErr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: rolegate: walk %s: %v\n", absRoot, walkErr)
		os.Exit(2)
	}

	// Constants may be declared in any file of the package, so resolve them after
	// the whole tree is parsed. Only same-package top-level constants are folded.
	defs := make(map[string][]constDef)
	for _, pf := range files {
		collectConsts(pf.file, pkgKey(pf.dir, pf.pkg), defs)
	}
	envs := make(map[string]map[string]string, len(defs))
	for key, list := range defs {
		envs[key] = resolveConsts(list)
	}

	seen := make(map[string]bool)
	var findings []finding
	for _, pf := range files {
		if !pf.isTest {
			continue
		}
		env := envs[pkgKey(pf.dir, pf.pkg)]
		ast.Inspect(pf.file, func(n ast.Node) bool {
			expr, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			switch expr.(type) {
			case *ast.BasicLit, *ast.Ident, *ast.BinaryExpr, *ast.ParenExpr, *ast.CallExpr:
			default:
				return true
			}
			value, ok := evalString(expr, env)
			if !ok || !clusterRoleSQL(value) {
				return true
			}
			pos := fset.Position(expr.Pos())
			key := fmt.Sprintf("%s:%d:%d", pos.Filename, pos.Line, pos.Column)
			if seen[key] {
				return true
			}
			seen[key] = true
			findings = append(findings, finding{
				file:  pos.Filename,
				line:  pos.Line,
				col:   pos.Column,
				value: value,
			})
			return true
		})
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		if findings[i].line != findings[j].line {
			return findings[i].line < findings[j].line
		}
		return findings[i].col < findings[j].col
	})
	for _, f := range findings {
		fmt.Printf("%s:%d: cluster role SQL must use pgxtest.NewRole (%s)\n", f.file, f.line, quoteValue(f.value))
	}

	if len(parseErrs) > 0 {
		for _, parseErr := range parseErrs {
			fmt.Fprintln(os.Stderr, parseErr)
		}
		fmt.Fprintln(os.Stderr, "FAIL: rolegate: unparseable Go source")
		os.Exit(2)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: rolegate: %d cluster-role write(s)\n", len(findings))
		os.Exit(1)
	}
}

func normalizeAllows(raw string) []string {
	var allows []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(strings.TrimPrefix(filepath.ToSlash(part), "./"))
		if part == "" {
			continue
		}
		if !strings.HasSuffix(part, "/") {
			part += "/"
		}
		allows = append(allows, part)
	}
	return allows
}

func isAllowed(rel string, allows []string) bool {
	for _, allow := range allows {
		if strings.HasPrefix(rel, allow) {
			return true
		}
	}
	return false
}

func pkgKey(dir, pkg string) string {
	return dir + "\x00" + pkg
}

func collectConsts(file *ast.File, key string, defs map[string][]constDef) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		var lastExpr ast.Expr
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if len(valueSpec.Values) == 0 {
				// `const ( a = "x"; b )` repeats the previous expression.
				if lastExpr == nil {
					continue
				}
				for _, name := range valueSpec.Names {
					addConst(defs, key, name.Name, lastExpr)
				}
				continue
			}
			lastExpr = valueSpec.Values[len(valueSpec.Values)-1]
			for i, name := range valueSpec.Names {
				expr := lastExpr
				if i < len(valueSpec.Values) {
					expr = valueSpec.Values[i]
				}
				addConst(defs, key, name.Name, expr)
			}
		}
	}
}

func addConst(defs map[string][]constDef, key, name string, expr ast.Expr) {
	if name == "_" {
		return
	}
	defs[key] = append(defs[key], constDef{name: name, expr: expr})
}

// resolveConsts folds string constants to a fixed point so later declarations may
// refer to earlier or later ones in the same package.
func resolveConsts(list []constDef) map[string]string {
	values := make(map[string]string, len(list))
	pending := append([]constDef(nil), list...)
	for len(pending) > 0 {
		var next []constDef
		progress := false
		for _, def := range pending {
			value, ok := evalString(def.expr, values)
			if !ok {
				next = append(next, def)
				continue
			}
			if previous, exists := values[def.name]; !exists || previous != value {
				values[def.name] = value
				progress = true
			}
		}
		if !progress {
			break
		}
		pending = next
	}
	return values
}

// evalString folds string literals, `+` concatenations, parentheses, same-package
// constants, and `string(x)` conversions. Unknown operands make the expression
// unevaluable rather than guessed.
func evalString(expr ast.Expr, consts map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return value, true
	case *ast.ParenExpr:
		return evalString(e.X, consts)
	case *ast.Ident:
		value, ok := consts[e.Name]
		return value, ok
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, leftOK := evalString(e.X, consts)
		right, rightOK := evalString(e.Y, consts)
		if !leftOK || !rightOK {
			return "", false
		}
		return left + right, true
	case *ast.CallExpr:
		if ident, ok := e.Fun.(*ast.Ident); ok && ident.Name == "string" && len(e.Args) == 1 {
			return evalString(e.Args[0], consts)
		}
	}
	return "", false
}

// clusterRoleSQL normalizes case and whitespace before matching so keywords split
// across concatenated literals are still caught. GRANT/REVOKE statements carrying
// ON are object privileges, not role membership changes.
func clusterRoleSQL(value string) bool {
	// Keep leading/trailing spaces: the retired regex required whitespace after
	// GRANT/REVOKE, so a prose suffix such as "idempotent revoke" must not match.
	normalized := spaceRun.ReplaceAllString(strings.ToUpper(value), " ")
	if normalized == "" {
		return false
	}
	if createRole.MatchString(normalized) || alterRole.MatchString(normalized) || dropRole.MatchString(normalized) {
		return true
	}
	if roleMembership(normalized, grantWord, toWord) || roleMembership(normalized, revokeWord, fromWord) {
		return true
	}
	return authMembers.MatchString(normalized)
}

func roleMembership(normalized string, keyword, terminal *regexp.Regexp) bool {
	for _, loc := range keyword.FindAllStringIndex(normalized, -1) {
		// The retired regex required whitespace after the keyword, so a prose
		// suffix ("idempotent revoke") or "GRANT;" is not role SQL.
		if loc[1] >= len(normalized) || normalized[loc[1]] != ' ' {
			continue
		}
		rest := strings.TrimLeft(normalized[loc[1]:], " ")
		if rest == "" {
			return true // keyword plus whitespace at end of string
		}
		if rest[0] == ';' {
			continue // `$` did not match before a statement separator
		}
		if end := strings.IndexByte(rest, ';'); end >= 0 {
			rest = rest[:end]
		}
		rest = strings.TrimSpace(rest)
		if onWord.MatchString(rest) {
			continue
		}
		if terminal.MatchString(rest) {
			return true
		}
	}
	return false
}

func quoteValue(value string) string {
	const limit = 120
	if len(value) > limit {
		value = value[:limit] + "..."
	}
	return strconv.Quote(value)
}
