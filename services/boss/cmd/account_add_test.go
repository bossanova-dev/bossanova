package main

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/spf13/cobra"
)

// handlersSource is the handler file's own source text, embedded so the
// assertion below holds under both `go test` and Bazel. Bazel does not stage a
// go_library's srcs into the test binary's runfiles, so parsing the path
// relative to the process cwd resolves only under `go test`.
//
//go:embed handlers.go
var handlersSource string

// findAddSubcommand returns the `add` subcommand of the account command group.
func findAddSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	account := accountCmd()
	for _, c := range account.Commands() {
		if c.Name() == "add" {
			return c
		}
	}
	t.Fatalf("account command has no `add` subcommand")
	return nil
}

func TestAccountAddCommandShape(t *testing.T) {
	add := findAddSubcommand(t)

	// New interactive-flow flags are present.
	for _, name := range []string{"token-stdin", "timeout"} {
		if add.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag on `account add`", name)
		}
	}
	// The BOS-160 non-interactive flags are preserved.
	for _, name := range []string{"provider", "label", "priority", "token", "credential-file"} {
		if add.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag to be preserved on `account add`", name)
		}
	}

	// --timeout defaults to 10m.
	if f := add.Flags().Lookup("timeout"); f != nil && f.DefValue != "10m0s" {
		t.Errorf("expected --timeout default 10m0s, got %q", f.DefValue)
	}

	// Args accepts at most one positional provider.
	if add.Args == nil {
		t.Fatalf("expected Args validator on `account add`")
	}
	if err := add.Args(add, []string{"claude"}); err != nil {
		t.Errorf("expected one positional arg to be accepted, got %v", err)
	}
	if err := add.Args(add, []string{"claude", "extra"}); err == nil {
		t.Errorf("expected two positional args to be rejected")
	}
}

// TestAccountAddClaudeSetsBothStdinFieldsFromTokenStdin pins the one place the
// PasteMode / StdinUnavailable split is still driven from a single boolean.
//
// accountflow deliberately separates the two (see ClaudeOptions): PasteMode says
// only "obtain the token by pasting rather than by running the CLI", while
// StdinUnavailable says "there is no interactive input left to read" and is what
// suppresses the label prompt. `--token-stdin` is both at once, so the CLI must
// set both. Dropping StdinUnavailable here leaves every existing test green —
// nothing else in this package reads the field — while headless
// `boss account add claude --token-stdin` starts resolving the label through
// Ask against a stdin the token already consumed.
//
// This is asserted over the source rather than by invoking the handler because
// runAccountAddClaude constructs its own daemon client and OS exec before it
// reaches accountflow; there is no seam to capture the options through, and
// adding one to a credential-carrying path is out of scope for this ticket.
func TestAccountAddClaudeSetsBothStdinFieldsFromTokenStdin(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers.go", handlersSource, 0)
	if err != nil {
		t.Fatalf("parse handlers.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "runAccountAddClaude" {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatal("runAccountAddClaude not found in handlers.go")
	}

	// The accountflow.ClaudeOptions literal the handler hands to RunClaudeAdd.
	fields := map[string]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ClaudeOptions" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if val, ok := kv.Value.(*ast.Ident); ok {
				fields[key.Name] = val.Name
			} else {
				fields[key.Name] = "<not an identifier>"
			}
		}
		return false
	})
	if len(fields) == 0 {
		t.Fatal("no accountflow.ClaudeOptions literal found in runAccountAddClaude")
	}

	for _, name := range []string{"PasteMode", "StdinUnavailable"} {
		got, present := fields[name]
		if !present {
			t.Errorf("ClaudeOptions.%s is not set from --token-stdin; the split needs BOTH "+
				"fields, and dropping this one silently re-prompts for the label against a spent stdin", name)
			continue
		}
		if got != "tokenStdin" {
			t.Errorf("ClaudeOptions.%s = %s, want it driven by the tokenStdin flag", name, got)
		}
	}
}
