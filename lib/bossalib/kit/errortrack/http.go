package errortrack

import (
	"net/http"

	sentryhttp "github.com/getsentry/sentry-go/http"
)

// HTTPHandler wraps the outermost HTTP handler so Sentry captures panics first,
// then re-panics for downstream recovery without flushing on the request path.
func HTTPHandler(next http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{
		Repanic:         true,
		WaitForDelivery: false,
	}).Handle(next)
}
