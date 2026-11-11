// Command testscan reports test fake declarations, sleeps and literal timer calls.
// It parses source only: build tags and unavailable dependencies do not hide tests.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type fake struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	File string `json:"file"`
	Line int    `json:"line"`
}
type sleep struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Call       string `json:"call"`
	InSynctest bool   `json:"inSynctest"`
}
type result struct {
	Fakes  []fake   `json:"fakes"`
	Sleeps []sleep  `json:"sleeps"`
	Errors []string `json:"errors"`
}

type binding struct {
	name       string
	start, end token.Pos
}

// A visitor value carries the lexical scope end. Recorded intervals let us resolve
// imports without type-checking any of the repository's independent Go modules.
type declarations struct {
	end      token.Pos
	bindings *[]binding
}

func (v declarations) add(id *ast.Ident, start token.Pos) {
	if id != nil {
		*v.bindings = append(*v.bindings, binding{id.Name, start, v.end})
	}
}
func (v declarations) fields(list *ast.FieldList, start token.Pos) {
	if list != nil {
		for _, field := range list.List {
			for _, id := range field.Names {
				v.add(id, start)
			}
		}
	}
}
func (v declarations) Visit(node ast.Node) ast.Visitor {
	switch n := node.(type) {
	case *ast.FuncDecl:
		if n.Body == nil {
			return nil
		}
		v.end = n.Body.End()
		v.fields(n.Recv, n.Body.Pos())
		v.fields(n.Type.Params, n.Body.Pos())
		v.fields(n.Type.Results, n.Body.Pos())
		v.fields(n.Type.TypeParams, n.Type.Pos())
	case *ast.FuncLit:
		v.end = n.Body.End()
		v.fields(n.Type.Params, n.Body.Pos())
		v.fields(n.Type.Results, n.Body.Pos())
	case *ast.BlockStmt:
		v.end = n.End()
	case *ast.IfStmt:
		v.end = n.End()
	case *ast.ForStmt:
		v.end = n.End()
	case *ast.SwitchStmt:
		v.end = n.End()
	case *ast.TypeSwitchStmt:
		v.end = n.End()
	case *ast.RangeStmt:
		v.end = n.End()
		if n.Tok == token.DEFINE {
			for _, expr := range []ast.Expr{n.Key, n.Value} {
				if id, ok := expr.(*ast.Ident); ok {
					v.add(id, n.Body.Pos())
				}
			}
		}
	case *ast.CaseClause:
		v.end = n.End()
	case *ast.CommClause:
		v.end = n.End()
	case *ast.AssignStmt:
		if n.Tok == token.DEFINE {
			for _, expr := range n.Lhs {
				if id, ok := expr.(*ast.Ident); ok {
					v.add(id, n.End())
				}
			}
		}
	case *ast.ValueSpec:
		for _, id := range n.Names {
			v.add(id, n.End())
		}
	case *ast.TypeSpec:
		v.add(n.Name, n.Name.Pos())
	}
	return v
}

func scanSource(fset *token.FileSet, file string, source any) result {
	out := result{Fakes: []fake{}, Sleeps: []sleep{}, Errors: []string{}}
	tree, err := parser.ParseFile(fset, file, source, parser.SkipObjectResolution)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return out
	}
	imports := map[string]string{}
	for _, imp := range tree.Imports {
		value, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if value != "time" && value != "testing/synctest" {
			continue
		}
		name := filepath.Base(value)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != "_" && name != "." {
			imports[name] = value
		}
	}
	var bindings []binding
	// Walk every expression, including function literals in package initializers.
	ast.Walk(declarations{end: tree.End(), bindings: &bindings}, tree)
	importedSelector := func(sel *ast.SelectorExpr, pkg string) bool {
		id, ok := sel.X.(*ast.Ident)
		if !ok || imports[id.Name] != pkg {
			return false
		}
		for _, b := range bindings {
			if b.name == id.Name && b.start <= id.Pos() && id.Pos() < b.end {
				return false
			}
		}
		return true
	}
	importedCall := func(call *ast.CallExpr, pkg, method string) bool {
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		return ok && sel.Sel.Name == method && importedSelector(sel, pkg)
	}
	importedTime := func(sel *ast.SelectorExpr) bool { return importedSelector(sel, "time") }
	type interval struct{ start, end token.Pos }
	var bubbles []interval
	ast.Inspect(tree, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && importedCall(call, "testing/synctest", "Test") && len(call.Args) > 0 {
			if fn, ok := ast.Unparen(call.Args[len(call.Args)-1]).(*ast.FuncLit); ok {
				bubbles = append(bubbles, interval{fn.Body.Pos(), fn.Body.End()})
			}
		}
		return true
	})
	ast.Inspect(tree, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.TypeSpec:
			if strings.HasPrefix(n.Name.Name, "fake") || strings.HasPrefix(n.Name.Name, "stub") || strings.HasPrefix(n.Name.Name, "mock") {
				out.Fakes = append(out.Fakes, fake{n.Name.Name, filepath.ToSlash(filepath.Dir(file)), file, fset.Position(n.Pos()).Line})
			}
		case *ast.CallExpr:
			call := ""
			for _, method := range []string{"Sleep", "After", "NewTimer", "AfterFunc", "Tick", "NewTicker"} {
				if importedCall(n, "time", method) && (method == "Sleep" || len(n.Args) > 0 && literalDuration(n.Args[0], importedTime)) {
					call = method
					break
				}
			}
			if call != "" {
				inside := false
				for _, bubble := range bubbles {
					if bubble.start <= n.Pos() && n.Pos() < bubble.end {
						inside = true
						break
					}
				}
				out.Sleeps = append(out.Sleeps, sleep{file, fset.Position(n.Pos()).Line, call, inside})
			}
		}
		return true
	})
	return out
}

func literalDuration(expr ast.Expr, importedTime func(*ast.SelectorExpr) bool) bool {
	switch n := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		return n.Kind == token.INT || n.Kind == token.FLOAT
	case *ast.SelectorExpr:
		if !importedTime(n) {
			return false
		}
		switch n.Sel.Name {
		case "Nanosecond", "Microsecond", "Millisecond", "Second", "Minute", "Hour":
			return true
		}
	case *ast.UnaryExpr:
		return literalDuration(n.X, importedTime)
	case *ast.BinaryExpr:
		return literalDuration(n.X, importedTime) && literalDuration(n.Y, importedTime)
	case *ast.CallExpr:
		sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Duration" && importedTime(sel) && len(n.Args) == 1 && literalDuration(n.Args[0], importedTime)
	}
	return false
}

func main() {
	out := result{Fakes: []fake{}, Sleeps: []sleep{}, Errors: []string{}}
	input := bufio.NewScanner(os.Stdin)
	fset := token.NewFileSet()
	for input.Scan() {
		file := input.Text()
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		var source any
		if len(os.Args) > 1 {
			contents, err := os.ReadFile(filepath.Join(os.Args[1], filepath.FromSlash(file)))
			if err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", file, err))
				continue
			}
			source = contents
		}
		found := scanSource(fset, file, source)
		out.Fakes = append(out.Fakes, found.Fakes...)
		out.Sleeps = append(out.Sleeps, found.Sleeps...)
		out.Errors = append(out.Errors, found.Errors...)
	}
	if err := input.Err(); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("read file list: %v", err))
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
