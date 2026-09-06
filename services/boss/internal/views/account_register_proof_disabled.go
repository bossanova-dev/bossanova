//go:build !e2e

package views

import "github.com/recurser/boss/internal/accountflow"

// proofRegisterExec is nil in every production build: the scripted login
// stand-in lives behind the e2e tag (account_register_proof.go) so no shipped
// binary carries a path that can substitute a synthetic credential for a real
// provider device login.
func proofRegisterExec() accountflow.Exec { return nil }
