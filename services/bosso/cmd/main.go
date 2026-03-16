// Package main is the entry point for the bosso orchestrator.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bosso: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("bosso: orchestrator stub")
	return nil
}
