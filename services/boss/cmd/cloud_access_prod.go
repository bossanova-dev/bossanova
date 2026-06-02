//go:build !e2e

package main

import "time"

func resolveE2ECloudAccessClient() cloudAccessClient {
	return nil
}

func e2eCloudRefreshInterval() time.Duration {
	return 0
}
