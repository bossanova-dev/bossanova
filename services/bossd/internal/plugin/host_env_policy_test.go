package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/recurser/bossalib/childenv"
)

// Keep the policy in step with the environment names production plugins read.
func TestPluginEnvironmentPolicyCoversLiteralReads(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "plugins", "bossd-plugin-claude")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("repository plugin sources unavailable")
		}
		root = parent
	}
	scanned, reads := 0, 0
	err = filepath.WalkDir(filepath.Join(root, "plugins"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join(root, "plugins"), path)
		if err != nil {
			return err
		}
		pluginName := strings.TrimPrefix(strings.Split(filepath.ToSlash(rel), "/")[0], "bossd-plugin-")
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		osAlias := ""
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"os"` {
				osAlias = "os"
				if imp.Name != nil {
					osAlias = imp.Name.Name
				}
			}
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != osAlias {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			reads++
			if len(childenv.Build([]string{name + "=test"}, childenv.ForPlugin(pluginName, nil))) != 1 {
				t.Errorf("%s reads %s outside its approved child environment", rel, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 || reads == 0 {
		t.Fatalf("source scan was empty: files=%d reads=%d", scanned, reads)
	}
}
