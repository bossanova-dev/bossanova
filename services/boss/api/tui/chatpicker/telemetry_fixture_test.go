package chatpicker

import (
	"context"

	"github.com/recurser/bossalib/kit/telemetry"
)

type fakeTelemetry struct {
	events      []telemetry.Event
	distinctIDs []string
	props       []map[string]any
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, distinctID string, props map[string]any) {
	f.events = append(f.events, event)
	f.distinctIDs = append(f.distinctIDs, distinctID)
	f.props = append(f.props, props)
}
func (f *fakeTelemetry) Identify(context.Context, string, map[string]any) {}
func (f *fakeTelemetry) Alias(context.Context, string, string)            {}
func (f *fakeTelemetry) Close()                                           {}
