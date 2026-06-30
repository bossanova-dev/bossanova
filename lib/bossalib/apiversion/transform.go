package apiversion

import "fmt"

// VersionChange describes a behavioral change associated with a specific API
// version. It down-converts a current-shape response to the behavior of the
// previous version for the RPC methods it targets; it must be a no-op for all
// other methods and message types.
//
// Real transforms in future PRs will type-assert msg against generated
// bossanova.v1 protobuf messages. The ReferenceChange in this file uses a
// simple demo struct to keep this package self-contained.
type VersionChange interface {
	// Version returns the API version at which this behavior change was
	// introduced. The transform is applied to any request resolved to a version
	// strictly older than Version().
	Version() Version

	// TransformResponse mutates msg in place to reflect the response shape of
	// the version prior to this change. method is the Connect RPC procedure
	// path (e.g. "/bossanova.v1.OrchestratorService/ListSessions").
	// Implementations must be no-ops for methods and message types they do not
	// target.
	TransformResponse(method string, msg any)
}

// Changes is an ordered list of VersionChanges (oldest→newest) that can be
// applied to a response message for a given resolved API version.
type Changes struct {
	reg     *Registry
	changes []VersionChange
}

// NewChanges constructs a Changes, validating that:
//   - every change's Version() is a member of reg
//   - changes are in non-decreasing version order (oldest→newest)
func NewChanges(reg *Registry, changes ...VersionChange) (*Changes, error) {
	for i, ch := range changes {
		v := ch.Version()
		if !reg.IsSupported(v) {
			return nil, fmt.Errorf("apiversion: change version %q is not in the registry", v)
		}
		if i > 0 {
			prev := changes[i-1].Version()
			if string(v) < string(prev) {
				return nil, fmt.Errorf("apiversion: changes must be in non-decreasing version order: %q < %q", v, prev)
			}
		}
	}
	return &Changes{reg: reg, changes: changes}, nil
}

// Apply runs, in newest→oldest order, every change whose Version() is strictly
// newer than resolved. A request resolved to the registry's Current version (or
// newer than all changes) runs zero transforms, keeping the hot path free of
// allocations.
func (c *Changes) Apply(method string, msg any, resolved Version) {
	for i := len(c.changes) - 1; i >= 0; i-- {
		ch := c.changes[i]
		if c.reg.Newer(ch.Version(), resolved) {
			ch.TransformResponse(method, msg)
		}
	}
}

// RefMsg is the demo message type used by ReferenceChange. Real transforms in
// future PRs will type-assert against generated bossanova.v1 protobuf messages;
// this demo struct keeps the apiversion package self-contained and is what the
// e2e test (a later task) drives.
type RefMsg struct {
	Greeting string
}

// ProductionChanges returns an empty Changes built against DefaultRegistry.
// The initial production registry contains a single version (Baseline), so
// there are no transforms to apply — zero transforms fire on all production
// traffic until a new Version is added and a VersionChange is appended here.
//
// Future API behavior changes should:
//  1. Append the new Version to DefaultRegistry (see version.go).
//  2. Set it as Current in DefaultRegistry.
//  3. Append a VersionChange entry here describing the down-convert transform.
//
// See docs/api-versioning.md for the full procedure.
func ProductionChanges() *Changes {
	c, err := NewChanges(DefaultRegistry())
	if err != nil {
		panic("apiversion: ProductionChanges is invalid: " + err.Error())
	}
	return c
}

// ReferenceChange is an example VersionChange introduced at V20260701. It
// demonstrates the VersionChange contract end-to-end and serves as the
// reference implementation for contributors adding real transforms.
//
// It targets the synthetic method "demo.Greet" and rewrites RefMsg.Greeting to
// prefix "[v1] ", simulating a backward-compatible response rewrite for clients
// pinned to the Baseline version. Real version changes follow this same pattern,
// type-asserting msg against generated bossanova.v1 message types.
type ReferenceChange struct{}

// Version implements VersionChange. The reference change was introduced at
// V20260701, so it is applied to any request resolved to Baseline.
func (ReferenceChange) Version() Version { return V20260701 }

// TransformResponse implements VersionChange. It acts only on the "demo.Greet"
// method with a *RefMsg payload; all other calls are no-ops.
func (ReferenceChange) TransformResponse(method string, msg any) {
	if method != "demo.Greet" {
		return
	}
	m, ok := msg.(*RefMsg)
	if !ok {
		return
	}
	m.Greeting = "[v1] " + m.Greeting
}
