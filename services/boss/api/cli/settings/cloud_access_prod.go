//go:build !e2e

package settings

import "time"

func ResolveE2ECloudAccessClient() cloudAccessClient {
	return nil
}

func E2ECloudRefreshInterval() time.Duration {
	return 0
}
