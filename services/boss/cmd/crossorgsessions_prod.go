//go:build !e2e

package main

import "github.com/recurser/boss/internal/client"

// applyE2ECrossOrgSessions is a no-op in production builds: the only thing that
// makes a real board read across organizations is the cloud client's
// cross-organization RPC. The e2e-tagged variant in crossorgsessions_e2e.go reads
// the BOSS_ORG_E2E_SESSION_* keys so a proof scenario can capture the union and
// the partial read without a cloud backend. Keeping the env reads behind the
// build tag means no environment variable can make a production boss withhold
// sessions it actually read.
func applyE2ECrossOrgSessions(c client.BossClient) client.BossClient { return c }
