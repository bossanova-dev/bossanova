package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog/log"
)

func (w *daemonWiring) wireRepairPlugin() error {
	// Auto-start the repair plugin synchronously. If the plugin is loaded
	// but its StartWorkflow fails, the daemon refuses to start: silently
	// continuing leaves auto-repair stopped (m.stopped=true) so every
	// subsequent NotifyStatusChange is dropped, which is exactly the
	// silent-fail mode that produced the empty repair-*.log files we found
	// on disk during the diagnose-first pass.
	//
	// GetInfo errors on individual plugins are tolerated (logged) so a
	// misbehaving non-repair workflow plugin can't gate the daemon.
	// Operators running without a repair plugin still get a healthy
	// daemon — auto-repair is opt-in by binary presence — but they get a
	// loud warning so the disabled state is visible.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var repairFound bool
		for _, svc := range w.pluginHost.GetWorkflowServices() {
			infoResp, err := svc.GetInfo(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("failed to get workflow plugin info; skipping for auto-start")
				continue
			}
			if infoResp == nil {
				log.Warn().Msg("workflow plugin returned nil info; skipping for auto-start")
				continue
			}
			if infoResp.Name != "repair" {
				continue
			}
			repairFound = true
			repairCfgJSON, err := json.Marshal(w.settings.Repair)
			if err != nil {
				cancel()
				return fmt.Errorf("marshal repair settings: %w", err)
			}
			log.Info().Str("plugin_name", infoResp.Name).Msg("auto-starting repair plugin")
			startReq := &bossanovav1.StartWorkflowRequest{
				ConfigJson: string(repairCfgJSON),
			}
			// Declare desired state FIRST: even if the synchronous start below
			// fails transiently, the health-tick watchdog now owns re-arming.
			// cfg-name and GetInfo-name coincide for repair ("repair", from
			// trimming bossd-plugin-); the desire map keys on cfg name.
			w.pluginHost.SetWorkflowDesired("repair", startReq)
			if _, started, err := w.pluginHost.EnsureWorkflowRunning(ctx, "repair"); err != nil {
				cancel()
				return fmt.Errorf("auto-start repair plugin: %w", err)
			} else if started {
				log.Info().Msg("repair plugin workflow armed")
			}
			log.Info().
				Str("plugin_name", infoResp.Name).
				Int("cooldown_minutes", w.settings.Repair.CooldownMinutes).
				Int("sweep_interval_minutes", w.settings.Repair.SweepIntervalMinutes).
				Int("idle_repair_threshold_minutes", w.settings.Repair.IdleRepairThresholdMinutes).
				Msg("repair plugin running")
		}
		cancel()
		if !repairFound {
			log.Warn().Msg("no repair plugin loaded; auto-repair is disabled until a bossd-plugin-repair binary is installed")
		}
	}

	return nil
}
