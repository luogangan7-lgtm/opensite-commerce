// Command privacywriters inventories OS file-creation call sites for the privacy
// registry. Depends on Go AST/types and the existing x/tools package loader;
// called only by privacy_registry.py in CI. An annotation belongs to one call,
// never every writer in a file; comments and strings cannot invent call sites.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/tools/go/packages"
)

type writer struct {
	Path      string   `json:"path"`
	Line      int      `json:"line"`
	Artifacts []string `json:"artifacts"`
}

var marker = regexp.MustCompile(`^// privacy-artifact: ([a-z][a-z0-9_]*\.[a-z][a-z0-9_]*)$`)

func inventory(root string) ([]writer, error) {
	result := []writer{}
	fset := token.NewFileSet()
	visited := map[string]bool{}
	visit := func(file *ast.File, info *types.Info) error {
		path := fset.Position(file.Pos()).Filename
		visited[path] = true
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		annotations := map[int][]string{}
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if match := marker.FindStringSubmatch(comment.Text); match != nil {
					line := fset.Position(group.End()).Line + 1
					annotations[line] = append(annotations[line], match[1])
				}
			}
		}
		counts := map[int]int{}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "CreateTemp", "OpenFile":
			default:
				return true
			}
			// A real module must type-resolve every call. Standalone synthetic
			// gate fixtures have no go.mod, so conservatively scan all four
			// selector names there (including unresolved os.Root receivers).
			// Preserve the old gate's conservative inventory of OpenFile
			// wrappers; resolving a non-os receiver must not exempt them.
			if info != nil && selector.Sel.Name != "OpenFile" {
				fn, ok := info.Uses[selector.Sel].(*types.Func)
				if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "os" {
					return true
				}
			}
			line := fset.Position(call.Pos()).Line
			counts[line]++
			result = append(result, writer{filepath.ToSlash(rel), line, annotations[line]})
			return true
		})
		for line, count := range counts {
			if count > 1 && len(annotations[line]) > 0 {
				return fmt.Errorf("MERCHANT_ARTIFACT_WRITER: %s:%d multiple calls share annotation", rel, line)
			}
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		loaded, err := packages.Load(&packages.Config{
			Dir:  root,
			Fset: fset,
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		}, "./internal/...")
		if err != nil {
			return nil, fmt.Errorf("MERCHANT_ARTIFACT_SCAN: package loading failed")
		}
		for _, pkg := range loaded {
			if len(pkg.Errors) > 0 {
				return nil, fmt.Errorf("MERCHANT_ARTIFACT_SCAN: package type check failed: %s", pkg.PkgPath)
			}
			for _, file := range pkg.Syntax {
				if err := visit(file, pkg.TypesInfo); err != nil {
					return nil, err
				}
			}
		}
	}
	// Preserve inventory of every source file, including inactive build tags.
	// Unloaded files use the conservative AST selector scan, never disappear.
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || visited[path] {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("MERCHANT_ARTIFACT_SCAN: invalid Go source")
		}
		return visit(file, nil)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "MERCHANT_ARTIFACT_SCAN: root argument required")
		os.Exit(1)
	}
	root, err := filepath.Abs(os.Args[1])
	if err == nil {
		var writers []writer
		writers, err = inventory(root)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(writers)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
