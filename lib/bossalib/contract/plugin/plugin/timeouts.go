package plugin

import "time"

// HeadlessCapabilityProbeTimeout bounds the cold-start CLI capability probe.
const HeadlessCapabilityProbeTimeout = 60 * time.Second

// HeadlessCapabilityPreflightRPCTimeout leaves time for plugin-side setup and
// response delivery after the headless capability probe reaches its own limit.
const HeadlessCapabilityPreflightRPCTimeout = HeadlessCapabilityProbeTimeout + 30*time.Second
