package main

import (
	"math"
	"os"
	"strconv"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/rs/zerolog"
)

// Type alias so the repair plugin references the shared client interface.
type hostClient = hostclient.Client

// newEagerHostServiceClient creates an eager host service client that dials
// the host service in the background via the go-plugin broker.
func newEagerHostServiceClient(broker *goplugin.GRPCBroker, logger zerolog.Logger) hostClient {
	return hostclient.NewEagerClient(broker, logger, hostclient.WithStartChatRunTimeout(startChatRunBudgetFromEnv()))
}

func startChatRunBudgetFromEnv() time.Duration {
	seconds, err := strconv.ParseInt(os.Getenv("BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey), 10, 64)
	if err != nil || seconds <= 0 {
		return config.StartChatRunBudgetFor(0)
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return config.StartChatRunBudgetFor(time.Duration(math.MaxInt64))
	}
	return config.StartChatRunBudgetFor(time.Duration(seconds) * time.Second)
}
