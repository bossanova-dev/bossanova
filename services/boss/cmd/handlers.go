package main

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/views"
)

// newClient creates a daemon client using the default socket path.
func newClient() (*client.Client, error) {
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}
	return client.New(socketPath), nil
}

func runTUI(_ *cobra.Command) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	app := views.NewApp(c)
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
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
