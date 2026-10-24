package server

import (
	"net/http"
	"net/http/pprof"
	"time"
)

// pprofPathPrefix is the route pprof is served under. Kept as a constant so the
// handler registration and the write-deadline override below cannot drift.
const pprofPathPrefix = "/debug/pprof/"

// registerPprofHandlers exposes net/http/pprof on the daemon's existing local
// socket (BOS-717).
//
// Diagnosing the 2026-08-06 wedge took an afternoon and still did not identify
// what blocked Create(), because bossd exposed no way to read its own goroutine
// state: `bossd --help` offers only -version, and `lldb -p` is refused by macOS
// hardened-runtime protection. A goroutine dump would have named the blocked
// frame in seconds.
//
// Exposure: this is registered ONLY on the Unix domain socket Listen creates,
// which is chmod 0600 and therefore reachable by the socket's owner alone. No
// TCP port is opened, so the "localhost-only" requirement is satisfied by the
// socket's file permissions rather than by a loopback bind. The connect handler
// keeps its socketauth bearer-token interceptor; pprof deliberately does not
// carry one, because an attacker who can open a 0600 socket owned by the user
// running bossd already has that user's privileges.
//
// Capture it with (the socket path is $BOSS_SOCKET, else settings.socket_path,
// else <app data dir>/bossd.sock — on macOS
// "~/Library/Application Support/bossanova/bossd.sock", on Linux
// ~/.config/bossanova/bossd.sock):
//
//	curl --unix-socket "$HOME/Library/Application Support/bossanova/bossd.sock" \
//	  'http://localhost/debug/pprof/goroutine?debug=2'
func registerPprofHandlers(mux *http.ServeMux) {
	// Index dispatches /debug/pprof/<name> for every runtime-registered profile
	// (goroutine, heap, allocs, block, mutex, threadcreate); the four below have
	// their own handlers because they are not runtime profiles.
	mux.Handle(pprofPathPrefix, withPprofWriteDeadlineOverride(http.HandlerFunc(pprof.Index)))
	mux.Handle(pprofPathPrefix+"cmdline", withPprofWriteDeadlineOverride(http.HandlerFunc(pprof.Cmdline)))
	mux.Handle(pprofPathPrefix+"profile", withPprofWriteDeadlineOverride(http.HandlerFunc(pprof.Profile)))
	mux.Handle(pprofPathPrefix+"symbol", withPprofWriteDeadlineOverride(http.HandlerFunc(pprof.Symbol)))
	mux.Handle(pprofPathPrefix+"trace", withPprofWriteDeadlineOverride(http.HandlerFunc(pprof.Trace)))
}

// withPprofWriteDeadlineOverride clears the daemon's 120s write deadline for
// pprof requests. A CPU profile defaults to 30s of sampling and callers routinely
// ask for more (`?seconds=120`), and a goroutine dump from a wedged daemon can be
// megabytes — either would be truncated mid-body by the server-wide WriteTimeout,
// producing a corrupt profile that is worse than none. Mirrors
// withCreateSessionWriteDeadlineOverride, which exists for the same reason on the
// streaming create path.
func withPprofWriteDeadlineOverride(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		next.ServeHTTP(w, r)
	})
}
