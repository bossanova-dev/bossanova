// Package main implements apiversioncheck, the post-deploy "the server must not
// trail its clients" gate. It asserts that a live bosso supports the API version
// this build's first-party clients request (apiversion.DefaultRegistry().Current()).
//
// It sends a single UNAUTHENTICATED, side-effect-free probe: a ReportBug unary
// (the one auth-exempt OrchestratorService method) carrying a deliberately
// out-of-range future Bossanova-Version header. The server's version interceptor
// rejects it before the handler runs and names its supported range; the check
// fails the release if the advertised supported-max is older than the built
// Current. That is exactly the BOS-241 skew — production deployed behind the
// versions its own main-built clients send — surfaced as a hard gate instead of
// a live outage. See docs/api-versioning.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/recurser/bossalib/apiversion"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func main() {
	// Delegate to run so deferred cleanup (context cancel) executes before exit.
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("apiversioncheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	url := flags.String("url", "", "target orchestrator base URL")
	timeout := flags.Duration("timeout", 15*time.Second, "request timeout")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		fmt.Fprintln(os.Stderr, "usage: apiversioncheck -url https://host [-timeout 15s]")
		return 2
	}

	current := apiversion.DefaultRegistry().Current()
	client := bossanovav1connect.NewOrchestratorServiceClient(http.DefaultClient, *url)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := checkCurrentSupported(ctx, client, current); err != nil {
		fmt.Fprintf(os.Stderr, "apiversioncheck: %v\n", err)
		return 1
	}
	fmt.Printf("PASS: server at %s supports built client API version %s\n", *url, current)
	return 0
}
