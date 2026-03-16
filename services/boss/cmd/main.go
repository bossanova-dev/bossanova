// Package main is the entry point for the boss CLI.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "boss: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("boss: CLI stub")
	return nil
}
