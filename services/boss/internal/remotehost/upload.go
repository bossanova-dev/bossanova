package remotehost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxUploadBytes caps the size of a file this package will push to the remote.
//
// The transport is a single ssh channel with the file on stdin, so there is no
// resume and no progress: a large file just looks like a hung paste. 10 MiB
// covers every screenshot and pasted image while keeping the worst case under a
// second on a normal link.
const MaxUploadBytes = 10 << 20

// ErrFileTooLarge means the local file exceeds MaxUploadBytes. It is a distinct
// sentinel because the remedy ("shrink it or use a different channel") differs
// from every other upload failure, and the UI wants to say so specifically.
var ErrFileTooLarge = errors.New("remotehost: file is too large to upload")

// defaultUploadBasename is used when sanitising leaves nothing usable. It is
// deliberately boring: the agent only needs a name it can refer to.
const defaultUploadBasename = "upload"

// maxRemoteBasenameLen bounds the sanitised basename so a pathological local
// name cannot push the remote path past a filesystem's per-component limit
// (255 bytes on ext4/APFS) and fail the write for a reason nobody can read.
const maxRemoteBasenameLen = 128

// uploadDirSuffix is the tail EnsureUploadDir's snippet appends to the remote
// cache root, and therefore the tail the remote must print back. It is one
// constant rather than two spellings so the snippet and the check on its output
// cannot drift apart.
const uploadDirSuffix = "/boss/uploads/"

// EnsureUploadDir creates (idempotently) the per-session remote upload directory
// and returns its ABSOLUTE remote path.
//
// The directory lives at `${XDG_CACHE_HOME:-$HOME/.cache}/boss/uploads/<key>`,
// under the remote user's own space rather than a shared /tmp: a world-readable
// /tmp path would let any other account on that host read images the user pasted
// into a private session. The leaf is created (and, if it already existed,
// forced) to mode 700 for the same reason.
//
// The round trip through the remote shell exists because the caller needs the
// LITERAL path — it goes into the agent's prompt, and an agent handed
// `$HOME/.cache/...` would look for a directory named `$HOME`. Only the remote
// side knows what $HOME is, so the snippet assigns, mkdirs, and prints the
// expanded value back.
//
// Quoting boundary: everything in the snippet except shellQuote(sessionKey) is
// CODE we wrote — the `${XDG_CACHE_HOME:-$HOME/.cache}` expansion is
// intentionally live, and must not be quoted or the remote shell would create a
// directory literally named `${XDG_CACHE_HOME:-$HOME/.cache}`. The session key
// is DATA, so it is single-quoted and concatenated onto the double-quoted
// prefix; the two quoting styles sit adjacent on purpose. The whole thing then
// goes through posixShell — see there for why the login shell cannot be trusted
// to parse any of it.
func EnsureUploadDir(ctx context.Context, opts Options, sessionKey string) (string, error) {
	if strings.TrimSpace(opts.Destination) == "" {
		return "", ErrNoDestination
	}
	if strings.TrimSpace(sessionKey) == "" {
		return "", fmt.Errorf("remotehost: no session key for the upload dir on %s", opts.Destination)
	}

	// The chmod re-asserts the mode on a directory that already existed, which
	// `mkdir -p` silently leaves alone. It takes no `--`: BSD/macOS chmod does
	// not accept one, and it needs none, because $dir is rooted at an absolute
	// $HOME (or $XDG_CACHE_HOME) and so can never begin with a dash.
	key := sanitizeSessionKey(sessionKey)
	snippet := `dir="${XDG_CACHE_HOME:-$HOME/.cache}` + uploadDirSuffix + `"` + shellQuote(key) +
		` && mkdir -p -m 700 -- "$dir" && chmod 700 "$dir" && printf '%s\n' "$dir"`

	stdout, stderr, err := runUploadSSH(ctx, opts, nil, posixShell(snippet))
	if err != nil {
		return "", fmt.Errorf("remotehost: could not create the upload dir on %s: %w%s",
			opts.Destination, err, stderrDetail(stderr))
	}

	dir := lastNonEmptyLine(stdout)
	// A login banner, an rc file that echoes, or a ForceCommand wrapper can put
	// its own lines on stdout ahead of ours, hence the last-line rule. Anything
	// that still is not absolute means the snippet never ran, and injecting a
	// relative path into the agent's prompt would be worse than failing here.
	if dir == "" || !strings.HasPrefix(dir, "/") {
		return "", fmt.Errorf("remotehost: %s did not report an absolute upload dir "+
			"(the remote shell printed no usable path)", opts.Destination)
	}
	// Clean before checking: a trailing slash on $XDG_CACHE_HOME yields a "//",
	// which is the same directory but not a clean path, and validateRemoteDir
	// (rightly) refuses those.
	dir = path.Clean(dir)
	// This value is the ONLY datum in the package that reaches a remote snippet
	// without having gone through a sanitiser — it is whatever the remote shell
	// printed. Everything downstream (UploadFile's mkdir, RemoveUploadDir's
	// `rm -rf`) re-quotes it and runs it, so pin it to the shape the snippet
	// above can produce and nothing else: the tail is fully known to us, so a
	// remote that reports anything else did not run our snippet, and an `rm -rf`
	// aimed at a directory we never created is not best-effort cleanup.
	if !strings.HasSuffix(dir, uploadDirSuffix+key) {
		return "", fmt.Errorf("remotehost: %s reported upload dir %q, which is not the %q "+
			"this session asked for (the remote shell did not run the snippet)",
			opts.Destination, dir, uploadDirSuffix+key)
	}
	if err := validateRemoteDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// UploadFile copies localPath into remoteDir and returns the absolute remote
// path of the copy.
//
// The bytes travel on the ssh command's stdin into a remote `cat`, not via
// scp/rsync: those would need their own destination parsing — the exact thing
// the package doc forbids, since a `~/.ssh/config` alias is not a scp
// `host:path` — and they would bypass the injected CommandFactory, so no test
// could observe them without a real host.
//
// Each upload gets its own subdirectory of remoteDir keyed by a random segment,
// with the sanitised original basename inside it. The subdirectory (not a
// mangled filename) carries the uniqueness so a second paste of `screenshot.png`
// cannot clobber the first while the agent still sees a name it can talk about.
func UploadFile(ctx context.Context, opts Options, localPath, remoteDir string) (string, error) {
	if strings.TrimSpace(opts.Destination) == "" {
		return "", ErrNoDestination
	}
	if err := validateRemoteDir(remoteDir); err != nil {
		return "", err
	}
	if strings.TrimSpace(localPath) == "" {
		return "", errors.New("remotehost: no local file to upload")
	}

	// Open first, then stat the open handle: statting the path and opening it
	// separately would leave a window where the file could change underneath.
	// filepath.Clean is applied last, as the gosec G304 guidance requires.
	file, err := os.Open(filepath.Clean(localPath))
	if err != nil {
		return "", fmt.Errorf("remotehost: cannot read %s: %w", localPath, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("remotehost: cannot stat %s: %w", localPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("remotehost: %s is not a regular file", localPath)
	}
	if info.Size() > MaxUploadBytes {
		return "", fmt.Errorf("%w: %s is %d bytes, over the %d-byte limit",
			ErrFileTooLarge, localPath, info.Size(), int64(MaxUploadBytes))
	}

	remoteSubdir := path.Join(remoteDir, uniqueSegment())
	remoteFile := path.Join(remoteSubdir, sanitizeRemoteBasename(filepath.Base(localPath)))
	// Belt and braces over sanitizeRemoteBasename: if that ever regressed, this
	// is the check that stops a hostile basename writing outside remoteDir.
	if !strings.HasPrefix(remoteFile, remoteDir+"/") {
		return "", fmt.Errorf("remotehost: refusing to write %s outside %s", remoteFile, remoteDir)
	}

	// Quoting boundary: the two paths are DATA and are single-quoted; `mkdir`,
	// `cat` and the redirect are CODE. `--` ends option parsing so a directory
	// whose name starts with a dash is not read as a flag. posixShell then keeps
	// the `&&` and the `>` out of the login shell's hands — fish parses both
	// differently. The file's bytes still reach the inner `cat`: `sh` inherits
	// the ssh channel's stdin and passes it down unchanged.
	snippet := "mkdir -p -m 700 -- " + shellQuote(remoteSubdir) +
		" && cat > " + shellQuote(remoteFile)

	// The Stat above is a fast reject, not the enforcement: it samples a size
	// that the writer can still grow. A screenshot tool is often STILL WRITING
	// when the user pastes, so a file that measured under the cap can exceed it
	// by the time these bytes are streamed — and the stream itself is what the
	// cap exists to bound. Cap the reader too, at one byte past the limit, so
	// overshoot is detectable rather than merely unlikely.
	capped := &io.LimitedReader{R: file, N: MaxUploadBytes + 1}

	_, stderr, err := runUploadSSH(ctx, opts, capped, posixShell(snippet))
	if err != nil {
		return "", fmt.Errorf("remotehost: could not upload %s to %s on %s: %w%s",
			filepath.Base(localPath), remoteSubdir, opts.Destination, err, stderrDetail(stderr))
	}
	// N is only safe to read on the success path: a nil error means Run also
	// finished copying stdin, whereas an expired WaitDelay can return from Wait
	// while the copying goroutine still holds this reader. Exhausting the extra
	// byte means the file outgrew the cap mid-upload; the remote copy is
	// truncated and must not be handed to the agent, and the session's upload
	// tree is removed wholesale at detach.
	if capped.N <= 0 {
		return "", fmt.Errorf("%w: %s grew past the %d-byte limit while it was being uploaded",
			ErrFileTooLarge, localPath, int64(MaxUploadBytes))
	}
	return remoteFile, nil
}

// RemoveUploadDir removes the per-session remote upload directory. Call sites
// treat it as best-effort — a dropped connection at detach must not block the
// user — but it still returns an error so the failure can be logged.
//
// The guard is deliberately stricter than "absolute": this issues `rm -rf` on
// someone else's machine, so a bug that let an empty, relative, unclean, or
// top-level path through would be a remote wipe rather than a leaked temp file.
func RemoveUploadDir(ctx context.Context, opts Options, remoteDir string) error {
	if strings.TrimSpace(opts.Destination) == "" {
		return ErrNoDestination
	}
	if err := validateRemoteDir(remoteDir); err != nil {
		return err
	}

	// The path is DATA and single-quoted; `--` stops rm reading a leading dash
	// as a flag. This one command needs no shell operators, so the login shell
	// could parse it — but it goes through posixShell anyway so all three
	// snippets have ONE quoting rule to reason about rather than two.
	_, stderr, err := runUploadSSH(ctx, opts, nil, posixShell("rm -rf -- "+shellQuote(remoteDir)))
	if err != nil {
		return fmt.Errorf("remotehost: could not remove the upload dir %s on %s: %w%s",
			remoteDir, opts.Destination, err, stderrDetail(stderr))
	}
	return nil
}

// validateRemoteDir rejects any remote directory this package is not willing to
// create in or delete. It requires an absolute, already-clean, whitespace-tight
// path with at least two components and no character that the two-layer quoting
// cannot carry — which rules out "", "~/...", "uploads/x", "/", "/home",
// anything carrying a "..", and anything carrying a quote or a backslash.
//
// It is the choke point for BOTH callers that splice a remote dir into a
// snippet, so every path that reaches a shell passes through here.
func validateRemoteDir(remoteDir string) error {
	dir := strings.TrimSpace(remoteDir)
	if dir == "" {
		return errors.New("remotehost: no remote upload dir")
	}
	// Validate the exact string the callers go on to quote and run. Trimming for
	// the checks and then splicing the untrimmed value would validate one path
	// and execute another.
	if dir != remoteDir {
		return fmt.Errorf("remotehost: remote upload dir %q has surrounding whitespace", remoteDir)
	}
	// The dir crosses TWO shell parses — the remote login shell, then the
	// `sh -c` body (see posixShell) — and that nesting only composes for data
	// free of quotes and backslashes. POSIX's `'\''` escape does NOT round-trip
	// through fish, which reads the backslash inside single quotes as an escape
	// and never closes the string; a remote $HOME of `/home/o'brien` makes every
	// emitted command exit 127 there. sanitizeSessionKey guarantees this for the
	// component we choose; this is the guarantee for the prefix the remote
	// reported, which no sanitiser ever saw. Control characters go too: a
	// newline in a remote command string is a command separator.
	if i := strings.IndexFunc(dir, unquotableRemoteRune); i >= 0 {
		// Decode rather than slice one byte: a rejected rune may be multi-byte
		// (C1 arrives as two bytes in UTF-8), and half of one is not a
		// diagnostic.
		bad, _ := utf8.DecodeRuneInString(dir[i:])
		return fmt.Errorf("remotehost: remote upload dir %q carries %q, which cannot be carried "+
			"through the remote shell quoting or into the agent's prompt", remoteDir, bad)
	}
	if !strings.HasPrefix(dir, "/") {
		// A leading "~" is the common mistake: ssh's remote shell would expand
		// it, but path.Join and the containment check below would not.
		return fmt.Errorf("remotehost: remote upload dir %q is not absolute", remoteDir)
	}
	if dir != path.Clean(dir) {
		return fmt.Errorf("remotehost: remote upload dir %q is not a clean path", remoteDir)
	}
	// path.Clean("/a/b") splits to ["", "a", "b"]; anything shallower than that
	// is a system directory, not something we created.
	if len(strings.Split(dir, "/")) < 3 {
		return fmt.Errorf("remotehost: remote upload dir %q is too close to the root", remoteDir)
	}
	return nil
}

// unquotableRemoteRune reports whether r must never appear in a value this
// package single-quotes into a remote snippet. Deliberately narrow — a space is
// fine inside single quotes in every shell, and remote home directories do
// legitimately contain them — so this rejects only what breaks the nested
// quoting or the terminal: the quote itself, the backslash, and every control
// character.
//
// "Every control character" includes C1 (U+0080–U+009F) and invalid UTF-8, not
// just C0 and DEL. This value is not merely quoted: it becomes the remote path,
// which internal/pty injects into the agent's composer with no sanitiser of its
// own — the sanitiser on that side guards the FAILURE text, not the success
// path. U+009B is CSI in an 8-bit locale, so a remote whose $HOME carried one
// could drive the user's terminal with no ESC in the string at all. Rejecting
// it here is where that closes, because here is where the value is first seen.
func unquotableRemoteRune(r rune) bool {
	switch {
	case r == '\'' || r == '\\':
		return true
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r == utf8.RuneError:
		// What IndexFunc decodes an invalid byte to. 0xFFFD is >= 0x20, so
		// without this an undecodable path would pass every check above.
		return true
	default:
		return false
	}
}

// posixShell wraps a POSIX snippet so it is interpreted by /bin/sh regardless of
// what the remote user's LOGIN shell is.
//
// `ssh host <command>` hands the command string to the login shell from the
// remote passwd entry, and that shell is not necessarily POSIX. fish is a real
// remote login shell — internal/views/attachtransport.go documents the same
// hazard for the tmux attach — and it parses NEITHER construct these snippets
// need: `dir=value` is "Unsupported use of '='" and `${VAR:-default}` is "${ is
// not a valid variable in fish". Both exit 127. Wrapping in `sh -c '…'` costs one
// process and makes the body's dialect ours rather than the remote user's; a
// single-quoted word is one of the few things every one of sh, bash, zsh, fish
// and csh agrees on.
//
// The wrapper adds a SECOND quoting layer over the one the snippet already
// applied to its data, and those two layers only compose because the data is
// restricted to a character class first — sanitizeSessionKey and
// sanitizeRemoteBasename for the components we choose, unquotableRemoteRune (via
// validateRemoteDir) for the remote directory the REMOTE reported.
//
// The escapes themselves are unavoidable: the snippet always contains single
// quotes (the quoted data, the literal `printf '%s\n'`), so the outer
// shellQuote always emits `'\”` runs. Those do survive fish, which closes the
// string, reads the `\'` outside it as a literal quote, and reopens — the same
// concatenation POSIX performs. The one place fish genuinely diverges is a
// BACKSLASH inside single quotes, which it reads as an escape where POSIX reads
// it literally, so the string never closes and the words re-split. That is not a
// cosmetic difference: an unsanitised key was observed executing `rm -rf ~` under
// a fish outer shell. Excluding the backslash (and the quote, which is what makes
// a stray backslash reachable in the first place) from the data is therefore
// load-bearing, not belt-and-braces — do not relax either without re-running
// Test*RunsUnderEveryRemoteLoginShell against a real fish.
func posixShell(snippet string) string {
	return "sh -c " + shellQuote(snippet)
}

// safeRemoteNameRune reports whether r may appear verbatim in a remote path
// component. The set is an allowlist on purpose: it has to survive two shell
// parses in an unknown dialect, so anything with meaning to ANY shell — quotes,
// backslashes, `$`, backticks, `!` (csh history expansion happens inside single
// quotes), glob characters, whitespace other than a plain space — is out.
func safeRemoteNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.' || r == '_' || r == '-':
		return true
	default:
		return false
	}
}

// sanitizeSessionKey reduces the caller's session key to one safe remote path
// component. The character policy is identical to sanitizeRemoteBasename's —
// both run safeRemoteNameRune, and neither keeps a space — so do not relax
// either on the belief that the other is the strict one. They differ only in
// that the basename also strips a directory part and pre-trims.
func sanitizeSessionKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		if safeRemoteNameRune(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return finishRemoteComponent(b.String())
}

// sanitizeRemoteBasename reduces an arbitrary local filename to a single safe
// remote path component. The agent is shown this name, so the goal is to keep it
// recognisable ("screenshot.png") while making it structurally incapable of
// escaping the upload directory, being read as a command-line flag, or carrying
// syntax through the two shell parses posixShell sets up.
//
// A space is NOT kept, even though it is inert inside the single quotes this
// package emits. The returned path's other consumer is the agent's PROMPT, and
// a space there is a token break: any path extractor truncates
// "…/Screenshot 2026-08-06 at 10.11.12.png" at "…/Screenshot".
// "Screenshot_2026-08-06_at_10.11.12.png" is just as recognisable and cannot be
// split, so the component we choose is made self-delimiting at the source.
//
// The directory prefix is a separate matter: it comes from the remote $HOME and
// may legitimately contain a space, which validateRemoteDir still allows. That
// residue is not ours to fix here — the pty layer escapes it at the injection
// site instead (see injectedRemotePath), which is belt to this braces rather
// than a replacement for it: keeping the name space-free means the common path
// needs no escaping at all.
func sanitizeRemoteBasename(name string) string {
	// Strip any directory part under BOTH separator conventions: the local file
	// may have come from a Windows-shaped string even when we run on POSIX, and
	// filepath.Base only knows the host's separator.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}

	// Trim BEFORE the allowlist pass, not after: finishRemoteComponent's own
	// trim can no longer see surrounding whitespace once it has become "_", so
	// an all-space name would survive as "___" instead of falling back.
	name = strings.TrimSpace(name)

	// Replace, rather than drop, every character outside the allowlist, so the
	// name stays recognisable and roughly the same length. This is not a
	// collision-avoidance scheme — "a b.png" and "a_b.png" both land on
	// "a_b.png"; uniqueness comes from uniqueSegment's per-upload subdirectory.
	var b strings.Builder
	for _, r := range name {
		if safeRemoteNameRune(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return finishRemoteComponent(b.String())
}

// finishRemoteComponent applies the rules both sanitisers share: no empty or
// traversal name, no leading dash, and a bounded length.
func finishRemoteComponent(s string) string {
	cleaned := strings.TrimSpace(s)
	// "." and ".." are the traversal names; anything else is inert as a single
	// component once the separators are gone.
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return defaultUploadBasename
	}
	// A leading dash would be parsed as an option by whatever the agent (or a
	// later shell command) runs over the path, even though our own snippets use
	// `--`.
	if strings.HasPrefix(cleaned, "-") {
		cleaned = "_" + cleaned[1:]
	}
	if len(cleaned) > maxRemoteBasenameLen {
		cleaned = strings.TrimSpace(cleaned[:maxRemoteBasenameLen])
	}
	return cleaned
}

// uniqueSegment returns the per-upload directory name. Random beats a counter
// because two boss processes can target the same remote session dir, and beats a
// timestamp alone because two pastes can land inside the same nanosecond tick on
// a coarse clock. The fallback keeps an upload working on a host with no usable
// entropy source rather than failing the paste.
func uniqueSegment() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

// lastNonEmptyLine returns the final non-blank line of stdout, trimmed. Remote
// stdout is not ours to constrain — a banner, an rc file's echo, or a
// ForceCommand wrapper can prepend lines — so the value we asked the remote to
// print last is the one to read.
func lastNonEmptyLine(stdout []byte) string {
	lines := strings.Split(string(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// batchModeOptions disables every interactive ssh credential prompt.
//
// Two of the three upload helpers run while an ATTACHED agent owns the terminal:
// boss has put it in raw mode and handed it to a full-screen process. ssh reads
// a passphrase or an `ssh-add -c` confirmation from /dev/tty, not stdin, and
// calls tcsetattr on it — so such a prompt paints over the agent's UI, flips the
// terminal modes out from under the PTY proxy, and cannot be answered, because
// the proxy's select(2) loop is the one reading the user's keystrokes. The
// upload then hangs behind an unanswerable question for the rest of the attach.
//
// The third, RemoveUploadDir, runs from a tea.Cmd after tea.Exec returned, so
// bubbletea owns the terminal again — a prompt there is behind the alt screen
// and equally unanswerable, and it would wedge a detach the user just asked for.
//
// BatchMode turns that into an immediate error, which pasteFailureMessage
// surfaces in the input box. Tunnel.HardenAuth does the same thing for the same
// reason once the TUI is live; discovery and the token fetch deliberately do NOT
// get this, because they run before the TUI and a passphrase prompt there is the
// user connecting normally.
var batchModeOptions = []string{"-o", "BatchMode=yes"}

// uploadSSHOptions is the ssh option list for the three upload helpers:
// BatchMode always, plus the session's multiplexing socket when there is one.
//
// The two are complementary rather than alternatives, which is why BatchMode
// stays even once a ControlPath is set. A slave handed a ControlPath whose
// master is gone — during a tunnel reconnect, or before the forward first came
// up — does not fail; ssh silently falls back to opening its own connection,
// which is exactly the case BatchMode exists to keep from wedging the pane. So
// ControlPath makes the prompt unnecessary on the happy path, and BatchMode
// still refuses it on the unhappy one.
//
// The returned slice is built by copy rather than by appending to the
// package-level batchModeOptions. That is defensive rather than a live fix:
// batchModeOptions is a full literal today (len == cap), so appending to it
// would reallocate and could not corrupt it — but the safety of that depends
// entirely on its length never changing, which is not a property a reader of
// this function can see. Copying costs one small allocation on a path that
// already spawns an ssh process, and removes the coupling.
func uploadSSHOptions(opts Options) []string {
	if opts.ControlPath == "" {
		return batchModeOptions
	}
	sshOptions := make([]string, 0, len(batchModeOptions)+2)
	sshOptions = append(sshOptions, batchModeOptions...)
	return append(sshOptions, "-o", "ControlPath="+opts.ControlPath)
}

// runUploadSSH is runSSHStdin for the calls that happen behind a live attached
// pane. See batchModeOptions and uploadSSHOptions.
func runUploadSSH(ctx context.Context, opts Options, stdin io.Reader, remoteArgs ...string) (stdout, stderr []byte, err error) {
	return runSSHOptions(ctx, opts, stdin, uploadSSHOptions(opts), remoteArgs)
}

// runSSHStdin is runSSH with a body: it invokes `ssh <destination>
// <remoteArgs...>` with stdin wired to the given reader (nil for none) and
// returns stdout and stderr separately. runSSH delegates here so the
// destination argv construction — the package's central invariant — has
// exactly one implementation.
func runSSHStdin(ctx context.Context, opts Options, stdin io.Reader, remoteArgs ...string) (stdout, stderr []byte, err error) {
	return runSSHOptions(ctx, opts, stdin, nil, remoteArgs)
}

// runSSHOptions is the single argv construction site for the runSSH* family —
// Tunnel builds its own long-lived `ssh -N` argv (tunnel.go), so a package-wide
// option added here does NOT reach the forward. sshOptions precede the
// destination because ssh stops parsing options at it: everything after the
// destination is the remote command.
func runSSHOptions(
	ctx context.Context,
	opts Options,
	stdin io.Reader,
	sshOptions, remoteArgs []string,
) (stdout, stderr []byte, err error) {
	args := make([]string, 0, len(sshOptions)+len(remoteArgs)+1)
	args = append(args, sshOptions...)
	// The destination is unmodified and identical across every invocation — see
	// the package doc. It follows any options, and is the last thing before the
	// remote command.
	args = append(args, opts.Destination)
	args = append(args, remoteArgs...)

	// #nosec G204 -- ssh with a user-supplied destination and a fixed remote command;
	// argv exec with no shell. owner=@recurser review-by=2027-02-07 issue=BOS-715
	// The destination is the user's own --host value (local trust), quoted for the
	// remote shell where needed.
	cmd := opts.commandFactory()(ctx, "ssh", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// Capturing both streams is what makes this necessary. os/exec gives a
	// non-*os.File Stdout/Stderr a pipe and a copying goroutine, and Run then
	// blocks until every process holding the write end closes it — which a
	// descendant of the ssh the context just killed (a ProxyCommand, a
	// multiplexer) need not do. Without the bound, cancelling ctx does not
	// reliably end Run, and the upload callers have nothing else to stop on:
	// PTYCommand.awaitPasteUploads joins its upload goroutines with no timeout
	// of its own, so an unbounded Run here freezes Ctrl+X detach. Same hazard,
	// same remedy, as the tunnel supervisor and the tmux liveness probe.
	//
	// This bounds only a cancelled or already-exited command, so the two
	// pre-TUI callers that still permit an interactive credential prompt
	// (discovery, the token fetch) are unaffected while that prompt is live.
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}
