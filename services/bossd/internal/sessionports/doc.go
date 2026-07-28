// Package sessionports discovers the listening TCP ports owned by a Bossanova
// session's process tree.
//
// # Pipeline
//
// A caller (BOS-472) supplies, per session, one or more Root processes — each
// a tmux pane PID paired with the OS birth Identity captured at the same
// instant. Detector.Scan then runs a single batched pass:
//
//  1. Snapshot the whole pid -> ppid graph once (production: `ps -axo
//     pid=,ppid=`).
//  2. Validate each root against its captured Identity, rejecting a PID reused
//     before the scan began.
//  3. Expand every valid root to its descendant tree, so overlapping and
//     nested roots attribute each process to the correct session(s) without
//     duplicate traversal or cross-session leakage.
//  4. Inspect listening sockets for the whole PID batch in one shot
//     (production: one lsof invocation on macOS, one procfs sweep on Linux).
//  5. Revalidate process identities AFTER socket inspection and attribute only
//     listeners whose entire ancestry chain stayed identity-stable across the
//     window.
//
// # Why identity revalidation
//
// /proc and lsof are inherently racy: a PID can be recycled between the moment
// we place it in a session's tree and the moment we read its sockets, which
// would otherwise attribute the replacement process's listeners to the wrong
// session. Capturing a process's kernel start time (sysctl KERN_PROC_PID on
// macOS, /proc/<pid>/stat field 22 on Linux) before and after socket
// inspection detects that reuse: a changed identity breaks the ancestry chain
// and the subtree beneath it is dropped.
//
// # Completeness, not errors
//
// Discovery degrades rather than failing. A global process-source failure or a
// cancellation returns incomplete, empty results for every requested session
// (never an error to the caller). A partial socket read keeps the listeners it
// did observe but marks only the affected sessions incomplete. SessionResult
// carries a Complete flag precisely so a transient "don't know" never removes
// ports a prior complete scan published — only Complete=true with no listeners
// is authoritative negative evidence.
//
// The package depends on nothing beyond the standard library, golang.org/x/sys,
// and zerolog: no protobuf, RPC/server handlers, HTTP probing, persistence, or
// TUI code. Platform specifics live behind build-tagged adapters
// (identity_darwin.go / identity_linux.go, socketsource_darwin.go /
// socketsource_linux.go) so the ownership logic is unit-tested OS-independently
// against fixtures.
package sessionports
