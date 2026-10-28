// proof-scenario inspects and validates input-only verification scenario documents.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/recurser/boss/internal/scenario"
)

type validationResult struct {
	OK       bool               `json:"ok"`
	Problems []scenario.Problem `json:"problems"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }

func run(args []string, out io.Writer) int {
	var result any
	status := 0
	switch {
	case len(args) > 0 && args[0] == "release":
		result, status = release(args[1:])
	case len(args) > 0 && args[0] == "hold":
		result, status = applyHold(args[1:])
	case len(args) > 0 && args[0] == "apply":
		result, status = apply(args[1:])
	case len(args) == 1 && args[0] == "schema":
		result = scenario.JSONSchema()
	case len(args) == 1 && args[0] == "capabilities":
		result = scenario.Capabilities()
	case len(args) == 2 && args[0] == "validate":
		data, err := os.ReadFile(args[1])
		problems := []scenario.Problem{}
		if err != nil {
			problems = append(problems, scenario.Problem{Path: "", Code: "required", Message: err.Error(), Hint: "Provide a readable scenario JSON file."})
		} else {
			_, problems = scenario.Decode(data)
		}
		result = validationResult{OK: len(problems) == 0, Problems: problems}
		if len(problems) > 0 {
			status = 1
		}
	default:
		result = map[string]any{"ok": false, "error": "usage: proof-scenario schema | capabilities | validate <file>"}
		status = 2
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return 1
	}
	return status
}

func apply(args []string) (any, int) {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts scenario.ApplyOptions
	var file string
	fs.StringVar(&file, "scenario", "", "scenario file")
	fs.StringVar(&opts.Home, "home", "", "run root")
	fs.StringVar(&opts.DB, "db", "", "run database")
	fs.StringVar(&opts.Socket, "socket", "", "run socket")
	fs.StringVar(&opts.SeedDB, "seeddb", "", "offline seed binary")
	fs.StringVar(&opts.Settings, "settings", "", "run settings")
	fs.StringVar(&opts.Phase, "phase", "offline", "prepare, offline or online")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return map[string]any{"ok": false, "problems": []scenario.Problem{{Code: "arguments", Message: "Invalid apply arguments", Hint: "Provide scenario, home, db, socket, seeddb and phase flags."}}}, 1
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return validationResult{Problems: []scenario.Problem{{Code: "required", Message: err.Error(), Hint: "Provide a readable scenario file."}}}, 1
	}
	doc, ps := scenario.Decode(data)
	if len(ps) > 0 {
		return validationResult{Problems: ps}, 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := scenario.Apply(ctx, doc, opts)
	if !result.OK {
		return result, 1
	}
	return result, 0
}

func applyHold(args []string) (any, int) {
	fs := flag.NewFlagSet("hold", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts scenario.ApplyOptions
	var file, ready string
	fs.StringVar(&file, "scenario", "", "scenario file")
	fs.StringVar(&ready, "ready", "", "readiness result file")
	fs.StringVar(&opts.Home, "home", "", "run root")
	fs.StringVar(&opts.DB, "db", "", "run database")
	fs.StringVar(&opts.Socket, "socket", "", "run socket")
	fs.StringVar(&opts.Settings, "settings", "", "run settings")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || ready == "" {
		return map[string]any{"ok": false, "error": "hold requires scenario, home, db, socket and ready flags"}, 2
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return validationResult{Problems: []scenario.Problem{{Code: "required", Message: err.Error(), Hint: "Provide a readable scenario file."}}}, 1
	}
	doc, ps := scenario.Decode(data)
	if len(ps) > 0 {
		return validationResult{Problems: ps}, 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result := scenario.Hold(ctx, doc, opts, ready)
	if !result.OK {
		return result, 1
	}
	return result, 0
}
func release(args []string) (any, int) {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "run root")
	if err := fs.Parse(args); err != nil || *home == "" {
		return map[string]any{"ok": false, "error": "release requires --home"}, 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := scenario.ReleaseOperations(ctx, *home, fs.Args())
	if !result.OK {
		return result, 1
	}
	return result, 0
}
