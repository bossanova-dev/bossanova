package agenterr

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/recurser/bossalib/appstate"
)

// redactedSentinel replaces every matched secret. It is exported as a
// sentinel value only in the sense that callers/tests may assert on its
// presence; the constant itself is unexported implementation detail.
const redactedSentinel = "[REDACTED]"

// fixturesSubdir is the appstate-relative name for the banner capture
// fixtures directory.
const fixturesSubdir = "cap-fixtures"

// captureSizeCap bounds a captured (already-redacted) snapshot to roughly the
// PostExit log-tail window (~8KB) so a runaway log can't fill the fixtures
// directory. When the redacted snapshot exceeds the cap, the trailing bytes are
// kept (banners land at the end of a log) and the leading bytes are dropped.
const captureSizeCap = 8 * 1024

// fixturesBaseDir resolves the base fixtures directory. It defaults to
// appstate.Path(fixturesSubdir) but is overridable (unexported package var)
// so tests can point it at a t.TempDir().
var fixturesBaseDir = func() (string, error) {
	return appstate.Path(fixturesSubdir)
}

// Regexes seeded from lib/bossalib/errortrack/scrub.go (approved,
// self-contained copy per BOS-164 Task 1 decision: agenterr does not import
// or refactor errortrack). This set is a deliberate *superset* of the
// errortrack patterns, tuned for the provider credential banners agenterr
// captures: it additionally redacts bare provider API keys (sk-… shapes) and
// broadens the env-secret label to any `*_KEY=`/bare `KEY=` assignment, not
// just `api_key`. Keeping this copy independent (rather than sharing) keeps the
// Sentry beforeSend path's blast radius at zero; the two scrubbers are expected
// to diverge in exactly this domain-specific direction.
var (
	reGitHubToken = regexp.MustCompile(`(?i)(?:github_pat_[A-Za-z0-9_]{20,}|(?:ghs|gho|ghp|ghu|ghr)_[A-Za-z0-9]{30,})`)
	reJWT         = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{20,}`)
	// rePEMPrivateKey redacts a whole PEM private-key block, including its
	// multi-line base64 body. The reEnvSecret value pattern below is
	// deliberately single-line (it stops at a newline to avoid over-consuming an
	// unterminated quote), so a `PRIVATE_KEY="-----BEGIN … KEY-----\n<body>…`
	// assignment would otherwise have only its first line redacted and leave the
	// real key material on the following lines. The (?s) flag lets `.` span
	// newlines and the non-greedy body matches to the nearest END marker; the
	// [A-Z0-9 ]* prefix/suffix covers labelled variants (RSA/EC/OPENSSH PRIVATE
	// KEY). Only PRIVATE KEY blocks are redacted — public CERTIFICATE/PUBLIC KEY
	// PEM blocks are not secret and are left intact.
	rePEMPrivateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// reProviderAPIKey matches bare provider API keys that appear in banner
	// prose without a `key=` prefix (e.g. "your key sk-ant-api03-… is invalid").
	// This is the canonical secret shape in Anthropic/OpenAI usage-and-auth
	// banners — exactly the text this package snapshots — so it must be redacted
	// even when it is not part of an Authorization header or env assignment. The
	// leading \b anchors the match to a token boundary so benign hyphenated prose
	// that merely contains the substring "sk-" (e.g. "task-", "risk-", "disk-")
	// is not corrupted — real keys always sit at a space/quote/`=` boundary.
	reProviderAPIKey = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)
	// reAuthHeader matches "Authorization: <scheme> <value>" for single-token
	// schemes (Basic, Bearer, Token) whose credential is a single non-whitespace
	// run. reAuthHeaderMulti below handles multi-parameter schemes (Digest,
	// MAC, AWS4-HMAC-SHA256) whose credential contains commas + spaces and
	// would otherwise be only partially redacted by \S+. reBearer is the
	// fallback for bare "Bearer <token>" occurrences without the prefix.
	reAuthHeader      = regexp.MustCompile(`(?i)(\bAuthorization:\s*(?:Basic|Bearer|Token)\s+)\S+`)
	reAuthHeaderMulti = regexp.MustCompile(`(?i)(\bAuthorization:\s*(?:Digest|MAC|AWS4-HMAC-SHA256)\s+)[^\r\n]+`)
	reBearer          = regexp.MustCompile(`(?i)(\bBearer\s+)[A-Za-z0-9._~+/\-]{20,}`)
	reEmail           = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// reEnvSecret matches `label = value` / `label: value` assignments whose
	// label denotes a secret. The label alternation covers `api_key`/`apikey`
	// plus any word ending in a separated or bare `key` (so `OPENAI_KEY=`,
	// `ANTHROPIC_API_KEY=`, and bare `KEY=` all match) as well as `secret`,
	// `token`, `password`, `passwd` — each optionally prefixed by one *or more*
	// underscore/hyphen-separated segments (`access_token=`, `MY_SECRET=`,
	// `AWS_SESSION_TOKEN=`, `MY_APP_SECRET=`). The repeated-segment prefix
	// `\b(?:[A-Za-z0-9]+[_-])*` uses `*` (not `?`) so multi-prefix env names are
	// still fully matched — anchoring only the first segment at a `\b` would
	// leave `AWS_SESSION_TOKEN=` unredacted. The `\b` still means a benign word
	// that merely ends in "key" (e.g. `monkey=banana`) is NOT matched, because
	// the zero-separator case leaves the alternation to match at the word start.
	//
	// The value part consumes a quoted string through its closing quote (or to
	// end of line if unterminated) before falling back to a bare non-space run,
	// so a quoted secret containing spaces — `PASSWORD="correct horse battery
	// staple"` or `TOKEN='abc def'` — is fully redacted rather than leaving the
	// tail tokens (`horse battery staple"`) in the fixture. The `(?:\\.|[^"\\\n])*`
	// body skips backslash-escaped characters, so an escaped quote inside a
	// JSON/config secret (`{"password":"abc\"def ghi"}`) is consumed through the
	// real closing quote instead of terminating at the escaped one and leaking the
	// `def ghi"` suffix into the fixture. The `\\.` alternative never spans a
	// newline (`.` excludes `\n`), so the match still stays on a single line, and
	// the trailing `"?` is optional so an unterminated opening quote still consumes
	// to end of line (fail closed).
	// The optional ["']? between the label and the separator matches JSON- and
	// config-style quoted keys ({"access_token":"ya29..."}, "client_secret" =
	// "...") where a closing quote sits between the label and the :/=. Without
	// it, common OAuth/JSON dumps fall through unredacted (the label's leading
	// \b already sits at the opening quote).
	reEnvSecret = regexp.MustCompile(`(?i)((?:api[_-]?key|\b(?:[A-Za-z0-9]+[_-])*(?:key|secret|token|password|passwd))["']?\s*[=:]\s*)(?:"(?:\\.|[^"\\\n])*"?|'(?:\\.|[^'\\\n])*'?|\S+)`)
)

// Redact strips known secret shapes from b, replacing each with
// redactedSentinel, and returns the redacted bytes. It cannot error; aside from
// reading the caller's home directory (os.UserHomeDir) for path redaction, its
// output depends only on its input. Patterns are applied in the same order as
// errortrack/scrub.go (with a whole-block PEM private-key pattern applied first,
// and the bare provider-API-key pattern inserted after the JWT pattern), using
// "${1}" capture-group preservation for the header/env patterns so the
// leading label survives redaction. Finally, the caller's home directory (if
// resolvable) is replaced with "~".
func Redact(b []byte) []byte {
	s := string(b)

	s = rePEMPrivateKey.ReplaceAllString(s, redactedSentinel)
	s = reGitHubToken.ReplaceAllString(s, redactedSentinel)
	s = reJWT.ReplaceAllString(s, redactedSentinel)
	s = reProviderAPIKey.ReplaceAllString(s, redactedSentinel)
	s = reAuthHeader.ReplaceAllString(s, "${1}"+redactedSentinel)
	s = reAuthHeaderMulti.ReplaceAllString(s, "${1}"+redactedSentinel)
	s = reBearer.ReplaceAllString(s, "${1}"+redactedSentinel)
	s = reEmail.ReplaceAllString(s, redactedSentinel)
	s = reEnvSecret.ReplaceAllString(s, "${1}"+redactedSentinel)

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}

	return []byte(s)
}

// CaptureBanner best-effort captures a redacted snapshot of an error banner
// to a fixtures file for later inspection, returning the written path. It
// never panics and never blocks; a write failure returns an error the caller
// may log-and-ignore.
//
// The snapshot is redacted (Redact) before anything is written to disk — this
// function fails closed and never writes an unredacted snapshot, even on a
// partial/best-effort basis. The redacted snapshot is then capped to the
// trailing captureSizeCap bytes so a runaway log can't fill the fixtures
// directory; capping after redaction (not before) keeps a secret straddling
// the cut boundary from leaking its unmatched suffix.
//
// The fixtures directory (appstate.Path("cap-fixtures")) only has its parent
// state directory created by appstate; CaptureBanner creates the
// "cap-fixtures" directory itself (0700) and writes the file 0600 inside it.
func CaptureBanner(kind Kind, snapshot []byte) (path string, err error) {
	base, err := fixturesBaseDir()
	if err != nil {
		return "", fmt.Errorf("resolve fixtures dir: %w", err)
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create fixtures dir %q: %w", base, err)
	}

	// Redact BEFORE capping, not after. Capping first would drop a straddling
	// secret's format prefix (the \b-anchored sk-/Bearer/KEY=/ghp_ head) into the
	// discarded leading bytes and leave its unmatched suffix in the retained
	// tail, writing partial secret material to disk — violating the fail-closed
	// guarantee. Redacting the whole snapshot first, then keeping the trailing
	// bytes, bounds output to captureSizeCap while ensuring every written byte
	// has passed through Redact (worst case a cosmetically truncated sentinel).
	redacted := capTail(Redact(snapshot), captureSizeCap)

	name, err := captureFilename(kind, time.Now())
	if err != nil {
		return "", fmt.Errorf("build capture filename: %w", err)
	}

	fullPath := filepath.Join(base, name)
	if err := os.WriteFile(fullPath, redacted, 0o600); err != nil {
		return "", fmt.Errorf("write capture file %q: %w", fullPath, err)
	}

	return fullPath, nil
}

// capTail returns the trailing at-most-n bytes of b. Banners land at the end
// of a log, so the tail (not the head) is the natural choice to keep when a
// snapshot exceeds the size cap.
func capTail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// captureFilename builds a filename with no secrets in it: a sortable
// timestamp, the classification kind, and a short random suffix to avoid
// collisions, e.g. "2006-01-02T15-04-05_usage_ab12cd.txt".
func captureFilename(kind Kind, at time.Time) (string, error) {
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	timestamp := at.Format("2006-01-02T15-04-05")
	return fmt.Sprintf("%s_%s_%s.txt", timestamp, captureFilenameKind(kind), hex.EncodeToString(suffix)), nil
}

// captureFilenameKind maps a Kind to the short token used in capture
// filenames. It reuses Kind.String() so the taxonomy and filenames never
// drift.
func captureFilenameKind(kind Kind) string {
	return kind.String()
}
