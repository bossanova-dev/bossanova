// Command mcp serves the bossanova MCP server. With no --http address it runs
// over stdio (the mode an MCP host spawns); with --http it runs as a persistent
// Streamable HTTP daemon (the mode launchd starts).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossanova-mcp/internal/serve"
	"github.com/recurser/bossanova-mcp/internal/socketbackend"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	httpAddr := fs.String("http", "", "Serve Streamable HTTP on this address (e.g. 127.0.0.1:8765); empty runs over stdio")
	readOnly := fs.Bool("read-only", false, "Register only read-only tools")
	socketPath := fs.String("socket", "", "Path to the bossd Unix socket (defaults to the configured socket path)")
	showVersion := fs.Bool("version", false, "Print build info and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// --version prints the on-disk binary's build metadata without starting the
	// server, so `boss mcp status` can compare it against the running service.
	if *showVersion {
		fmt.Println(buildinfo.String())
		return nil
	}

	resolvedSocket := *socketPath
	if resolvedSocket == "" {
		p, err := socketbackend.DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("resolve socket path: %w", err)
		}
		resolvedSocket = p
	}

	backend, err := socketbackend.New(resolvedSocket)
	if err != nil {
		return fmt.Errorf("create backend: %w", err)
	}

	opts := bossmcp.Options{ReadOnly: *readOnly}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *httpAddr == "" {
		return serve.Stdio(ctx, backend, opts)
	}
	return serve.HTTP(ctx, *httpAddr, backend, opts)
}
