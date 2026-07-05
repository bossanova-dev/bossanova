package main

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// probeVersion is a valid YYYY-MM-DD date placed far in the future so it is
// never a member of any registry. The server's version interceptor therefore
// rejects it with the supported-range error BEFORE the ReportBug handler runs,
// which makes the probe side-effect-free: no bug report is ever created.
const probeVersion = "9999-12-31"

// bugReporter is the one auth-exempt OrchestratorService method
// (services/bosso/internal/auth/middleware.go). The generated
// OrchestratorServiceClient satisfies it; tests supply a fake so the range
// parsing and skew comparison are exercised without a live server.
type bugReporter interface {
	ReportBug(context.Context, *connect.Request[pb.ReportBugRequest]) (*connect.Response[pb.ReportBugResponse], error)
}

// supportedMaxFromError extracts MAX from a version-interceptor error of the
// documented form "...; supported range: MIN to MAX" (see
// lib/bossalib/apiversion/interceptor.go). It returns ok=false when the marker
// is absent or MAX is not a parseable version, so the caller fails closed rather
// than guessing.
func supportedMaxFromError(errText string) (apiversion.Version, bool) {
	_, rest, ok := strings.Cut(errText, "supported range: ")
	if !ok {
		return "", false
	}
	_, maxRaw, ok := strings.Cut(rest, " to ")
	if !ok {
		return "", false
	}
	maxPart := strings.TrimSpace(maxRaw)

	// The MAX token is a date; stop at the first character that cannot be part of
	// a YYYY-MM-DD string so trailing punctuation/text (quotes, newlines) is cut.
	end := len(maxPart)
	for k, r := range maxPart {
		if r != '-' && (r < '0' || r > '9') {
			end = k
			break
		}
	}
	v, err := apiversion.Parse(maxPart[:end])
	if err != nil {
		return "", false
	}
	return v, true
}

// checkCurrentSupported sends the side-effect-free out-of-range probe and
// verifies the live server advertises a supported-max at least as new as the
// built client version. A server whose max trails current is the BOS-241 skew:
// production has fallen behind the versions its own main-built clients request.
func checkCurrentSupported(ctx context.Context, r bugReporter, current apiversion.Version) error {
	req := connect.NewRequest(&pb.ReportBugRequest{})
	req.Header().Set(apiversion.HeaderName, probeVersion)

	_, err := r.ReportBug(ctx, req)
	if err == nil {
		// The interceptor must reject an out-of-range version before the handler
		// runs. A success means the server has no version interceptor (predates API
		// versioning) or accepted an impossible version — either way the supported
		// range is unverifiable, so fail closed.
		return fmt.Errorf("server accepted out-of-range probe version %q; cannot verify supported range (server may predate API versioning)", probeVersion)
	}

	max, ok := supportedMaxFromError(err.Error())
	if !ok {
		return fmt.Errorf("could not parse supported version range from server response: %w", err)
	}

	// Dates are zero-padded YYYY-MM-DD, so lexicographic ordering is chronological.
	if string(max) < string(current) {
		return fmt.Errorf(
			"server supported-max %q trails the built client version %q: production is behind its own clients — deploy the server supporting %q before (or with) any client that requests it (docs/api-versioning.md, BOS-241)",
			max, current, current)
	}
	return nil
}
