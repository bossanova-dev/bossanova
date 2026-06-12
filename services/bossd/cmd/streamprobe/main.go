// Package main implements streamprobe, a full-duplex HTTP/2 transport check.
// It sends an unauthenticated Connect stream and uses rejection/header timing
// to distinguish immediate response headers from middleboxes that hold them
// until request-body EOF.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	flags := flag.NewFlagSet("streamprobe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	url := flags.String("url", "", "target orchestrator base URL")
	wait := flags.Duration("wait", 6*time.Second, "time to wait for response headers")
	insecure := flags.Bool("insecure", false, "skip TLS certificate verification")

	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *url == "" {
		fmt.Fprintln(os.Stderr, "usage: streamprobe -url https://host [-wait 6s] [-insecure]")
		os.Exit(2)
	}

	res, err := probeDuplex(*url, *wait, *insecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "streamprobe: %v\n", err)
		os.Exit(2)
	}

	fmt.Println(formatResult(res))
	if !res.Duplex {
		os.Exit(1)
	}
}
