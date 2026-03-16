// Package main is the entry point for the bossd daemon.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bossd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("bossd: daemon stub")
	return nil
}
