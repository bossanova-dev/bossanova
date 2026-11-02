package main

import (
	"os"
	"testing"

	"github.com/99designs/keyring"

	"github.com/recurser/bossd/internal/adapter/accountcred"
)

// TestMain keeps every daemon started by this package's tests off the OS
// keyring: startup persists the refresh broker endpoint, and a test run must
// never overwrite the endpoint state of a developer's live daemon.
func TestMain(m *testing.M) {
	newAccountCredentialStore = func() *accountcred.Store {
		return accountcred.NewWithKeyring(keyring.NewArrayKeyring(nil))
	}
	os.Exit(m.Run())
}
