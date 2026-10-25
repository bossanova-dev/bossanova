// Command gometrics reports source-level facts without resolving dependencies or build tags.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type interfaceMetrics struct {
	Name     string `json:"name"`
	Exported bool   `json:"exported"`
	Methods  int    `json:"methods"`
	Embeds   int    `json:"embeds"`
}

type testMetrics struct {
	Name     string   `json:"name"`
	Subtests []string `json:"subtests"`
}

type fileMetrics struct {
	File       string             `json:"file"`
	Dir        string             `json:"dir"`
	Pkg        string             `json:"pkg"`
	Generated  bool               `json:"generated"`
	Test       bool               `json:"test"`
	Lines      int                `json:"lines"`
	Exported   int                `json:"exported"`
	Interfaces []interfaceMetrics `json:"interfaces"`
	Tests      []testMetrics      `json:"tests"`
}

func scanSource(file string, source []byte) (fileMetrics, error) {
	out := fileMetrics{
		File: file, Dir: filepath.ToSlash(filepath.Dir(file)),
		Test: strings.HasSuffix(file, "_test.go"), Lines: bytes.Count(source, []byte{'\n'}),
		Interfaces: []interfaceMetrics{}, Tests: []testMetrics{},
	}
	tree, err := parser.ParseFile(token.NewFileSet(), file, source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return out, err
	}
	out.Pkg = tree.Name.Name
	out.Generated = ast.IsGenerated(tree)
	for _, declaration := range tree.Decls {
		switch decl := declaration.(type) {
		case *ast.FuncDecl:
			if decl.Recv != nil {
				continue
			}
			if decl.Name.IsExported() {
				out.Exported++
			}
			if out.Test && strings.HasPrefix(decl.Name.Name, "Test") {
				out.Tests = append(out.Tests, testMetrics{Name: decl.Name.Name, Subtests: subtestNames(decl)})
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch item := spec.(type) {
				case *ast.ValueSpec:
					for _, name := range item.Names {
						if name.IsExported() {
							out.Exported++
						}
					}
				case *ast.TypeSpec:
					if item.Name.IsExported() {
						out.Exported++
					}
					if iface, ok := item.Type.(*ast.InterfaceType); ok {
						metric := interfaceMetrics{Name: item.Name.Name, Exported: item.Name.IsExported()}
						for _, field := range iface.Methods.List {
							if len(field.Names) == 0 {
								metric.Embeds++
							} else {
								metric.Methods += len(field.Names)
							}
						}
						out.Interfaces = append(out.Interfaces, metric)
					}
				}
			}
		}
	}
	return out, nil
}

func subtestNames(decl *ast.FuncDecl) []string {
	names := []string{}
	if decl.Body == nil {
		return names
	}
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Run" {
			return true
		}
		receiver, ok := sel.X.(*ast.Ident)
		if !ok || receiver.Name != "t" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(literal.Value)
		if err == nil {
			names = append(names, name)
		}
		return true
	})
	return names
}

func run() error {
	root := flag.String("root", ".", "repository containing the input paths")
	flag.Parse()
	rows := []fileMetrics{}
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		file := input.Text()
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(*root, filepath.FromSlash(file)))
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		row, err := scanSource(file, source)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	if err := input.Err(); err != nil {
		return fmt.Errorf("read file list: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(rows)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
