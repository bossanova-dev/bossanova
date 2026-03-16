package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Handler stubs — implemented in subsequent tasks.

func runTUI(_ *cobra.Command) error {
	fmt.Println("boss: interactive TUI (not yet implemented)")
	return nil
}

func runLS(_ *cobra.Command) error {
	fmt.Println("boss ls: list sessions (not yet implemented)")
	return nil
}

func runNew(_ *cobra.Command) error {
	fmt.Println("boss new: create session (not yet implemented)")
	return nil
}

func runAttach(_ *cobra.Command, _ string) error {
	fmt.Println("boss attach: attach to session (not yet implemented)")
	return nil
}

func runRepoAdd(_ *cobra.Command) error {
	fmt.Println("boss repo add: add repository (not yet implemented)")
	return nil
}

func runRepoLS(_ *cobra.Command) error {
	fmt.Println("boss repo ls: list repositories (not yet implemented)")
	return nil
}

func runRepoRemove(_ *cobra.Command, _ string) error {
	fmt.Println("boss repo remove: remove repository (not yet implemented)")
	return nil
}

func runArchive(_ *cobra.Command, _ string) error {
	fmt.Println("boss archive: archive session (not yet implemented)")
	return nil
}

func runResurrect(_ *cobra.Command, _ string) error {
	fmt.Println("boss resurrect: resurrect session (not yet implemented)")
	return nil
}

func runTrashEmpty(_ *cobra.Command) error {
	fmt.Println("boss trash empty: empty trash (not yet implemented)")
	return nil
}
