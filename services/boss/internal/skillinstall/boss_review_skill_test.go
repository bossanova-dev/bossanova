package skillinstall

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type falsificationProsePin struct {
	name         string
	pattern      string
	live         string
	tokenRemoved string
	// Additional single-anchor mutations, each of which must ALSO fail the pattern. A pin whose
	// `tokenRemoved` mutates several anchors at once proves only that the pattern rejects the
	// combination — it cannot tell a pattern that requires every anchor from one that requires just
	// the first. Listing the mutations separately makes each anchor independently load-bearing.
	alsoRemoved []string
}

// allProsePins is the enrolment registry TestFalsificationProsePinsAreNonVacuous ranges over.
// Declaring a pin through regProsePin/regProsePins is what enrols it, so the harness can no longer
// learn about pins from a list somebody has to remember to extend. BOS-798: the previous literal
// enumeration had silently omitted three declared pins, and one of them —
// falsificationHostNativeHandoffPin — carried a `live` fixture that could not match its own
// pattern. It was still asserted against real prose, so nothing was red; its pattern was simply
// never checked for self-consistency. An unvalidated pin is the vacuous pin this harness exists to
// catch, so enrolment must be structural rather than remembered.
var allProsePins []falsificationProsePin

func regProsePin(pin falsificationProsePin) falsificationProsePin {
	allProsePins = append(allProsePins, pin)
	return pin
}

func regProsePins(pins []falsificationProsePin) []falsificationProsePin {
	allProsePins = append(allProsePins, pins...)
	return pins
}

// falsificationStepPins is deliberately NOT registered on its own: falsificationReferencePins
// clones it wholesale, so enrolling both would run every step pin twice.
var falsificationStepPins = []falsificationProsePin{
	{
		name:         "name-property",
		pattern:      `Name\s+the\s+property`,
		live:         "Name the property",
		tokenRemoved: "Name the claim",
	},
	{
		name:         "mutate-production-feed",
		pattern:      `Mutate\s+the\s+production\s+feed,\s+never\s+the\s+assertion`,
		live:         "Mutate the production feed, never the assertion",
		tokenRemoved: "Mutate the production feed, never the text",
	},
	{
		name:         "prove-mutation-landed",
		pattern:      `Prove\s+the\s+mutation\s+landed`,
		live:         "Prove the mutation landed",
		tokenRemoved: "Prove the command completed",
	},
	{
		name:         "red-right-reason",
		pattern:      `Require\s+red\s+for\s+the\s+right\s+reason`,
		live:         "Require red for the right reason",
		tokenRemoved: "Require red for a reason",
	},
	{
		name:         "restore-and-prove",
		pattern:      `Restore\s+exactly,\s+then\s+prove\s+the\s+restore`,
		live:         "Restore exactly, then prove the restore",
		tokenRemoved: "Restore exactly, then finish",
	},
}

var falsificationReferencePins = regProsePins(append(slicesClone(falsificationStepPins), []falsificationProsePin{
	{
		name:         "one-sided-unbounded-direction",
		pattern:      `one-sided\s+bound.*direction\s+it\s+does\s+not\s+bound`,
		live:         "one-sided bound must name the direction it does not bound",
		tokenRemoved: "one-sided bound must name its direction",
	},
	{
		name:         "mutation-numstat",
		pattern:      `git\s+diff\s+--numstat\s+--\s+"<absolute\s+path>".*must\s+be\s+non-empty`,
		live:         "git diff --numstat -- \"<absolute path>\" must be non-empty",
		tokenRemoved: "git diff --numstat must be non-empty",
	},
	{
		name:         "mutation-cmp",
		pattern:      `cmp\s+-s.*must\s+fail`,
		live:         "cmp -s on the originals must fail",
		tokenRemoved: "cmp -s on the originals must run",
	},
	{
		name:         "mutation-cmp-requires-difference-exit-one",
		pattern:      `cmp\s+-s.*exit\s+1.*inputs\s+differ.*exit\s+2.*harness\s+error`,
		live:         "cmp -s must exit 1: inputs differ; exit 2 is a harness error",
		tokenRemoved: "cmp -s must exit 1: inputs differ",
	},
	{
		name:         "wrong-red-is-not-kill",
		pattern:      `compile\s+error.*harness\s+error.*not\s+a\s+kill`,
		live:         "a compile error or harness error is not a kill",
		tokenRemoved: "a compile error or harness error is a kill",
	},
	{
		name:         "rerun-green",
		pattern:      `re-run\s+the\s+gate\s+green`,
		live:         "re-run the gate green",
		tokenRemoved: "re-run the build green",
	},
	{
		name:         "tier-a-heading",
		pattern:      `Tier\s+A\s+—\s+zero-write\s+probe`,
		live:         "Tier A — zero-write probe",
		tokenRemoved: "Tier A — probe",
	},
	{
		name:         "tier-b-heading",
		pattern:      `Tier\s+B\s+—\s+in-place\s+mutation`,
		live:         "Tier B — in-place mutation",
		tokenRemoved: "Tier B — mutation",
	},
	{
		name:         "reviewer-tier-a-only",
		pattern:      `read-only\s+reviewer.*Tier\s+A\s+only`,
		live:         "A read-only reviewer may use Tier A only",
		tokenRemoved: "A read-only reviewer may use either tier",
	},
	{
		name:         "tier-b-restricted",
		pattern:      `Tier\s+B.*orchestrator,\s+fix,\s+and\s+repair\s+paths\s+only`,
		live:         "Tier B is for orchestrator, fix, and repair paths only",
		tokenRemoved: "Tier B is for all paths",
	},
	{
		name:         "go-overlay",
		pattern:      `go\s+test\s+-overlay.*\x60?\$PROBE_DIR\x60?\s+replacement\s+file`,
		live:         "go test -overlay with a $PROBE_DIR replacement file",
		tokenRemoved: "go test with a $TMPDIR replacement file",
	},
	{
		name:         "go-overlay-json-manifest",
		pattern:      `JSON\s+overlay\s+manifest.*Replace.*absolute\s+original\s+path.*absolute\s+replacement\s+source\s+path.*env\s+-i.*go\s+test\s+-mod=readonly\s+-overlay\s+"\$PROBE_DIR/overlay\.json"`,
		live:         "JSON overlay manifest with Replace mapping the absolute original path to the absolute replacement source path, then env -i go test -mod=readonly -overlay \"$PROBE_DIR/overlay.json\"",
		tokenRemoved: "JSON overlay manifest with Replace mapping the absolute original path to the absolute replacement source path",
	},
	{
		name:         "go-overlay-read-only-module-cache",
		pattern:      `(?s)GO_MOD_CACHE=\$\(go\s+env\s+GOMODCACHE\).*GO_MOD_CACHE.*mounted\s+read-only.*GOMODCACHE="\$GO_MOD_CACHE"`,
		live:         "GO_MOD_CACHE=$(go env GOMODCACHE), with GO_MOD_CACHE mounted read-only, then GOMODCACHE=\"$GO_MOD_CACHE\"",
		tokenRemoved: "GOMODCACHE=\"$PROBE_DIR/go-modcache\"",
	},
	{
		name:         "tier-a-private-probe-root",
		pattern:      `Tier\s+A.*private\s+per-probe\s+scratch\s+root.*PROBE_DIR\s*=\s*\$\(mktemp\s+-d\s+"\$TMP_ROOT/boss-review-tier-a\.XXXXXX"\)`,
		live:         "Tier A creates a private per-probe scratch root: PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-tier-a.XXXXXX\")",
		tokenRemoved: "Tier A creates a scratch root: PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-tier-a.XXXXXX\")",
	},
	{
		name:         "tier-a-scratch-root-outside-checkout",
		pattern:      `relative\s+\x60?\$TMPDIR\x60?.*not\s+a\s+trusted\s+root.*CHECKOUT_ROOT_OUTPUT\s*=\s*\$\(git\s+rev-parse\s+--show-toplevel.*TMP_ROOT_OUTPUT\s*=\s*\$\(cd\s+"\$TMP_ROOT".*printf\s+'?%sx'?\s+"\$PWD"\).*TMP_ROOT=\$\{TMP_ROOT_OUTPUT%x\}.*Tier\s+A\s+temporary\s+root\s+is\s+inside\s+the\s+checkout.*Tier\s+A\s+scratch\s+root\s+is\s+inside\s+the\s+checkout`,
		live:         "A relative $TMPDIR is not a trusted root. CHECKOUT_ROOT_OUTPUT=$(git rev-parse --show-toplevel && printf x); TMP_ROOT_OUTPUT=$(cd \"$TMP_ROOT\" && printf '%sx' \"$PWD\"); TMP_ROOT=${TMP_ROOT_OUTPUT%x}; Tier A temporary root is inside the checkout; Tier A scratch root is inside the checkout",
		tokenRemoved: "A relative $TMPDIR can be used as a scratch root",
	},
	{
		name: "tier-a-cleans-up-on-cancellation",
		// The status travels in PROBE_STATUS rather than as `cleanup_probe_dir $?`: this recipe ships
		// inside a skill that is also reachable as a slash command, and the harness rewrites `$1`-`$9`
		// in a skill body before any shell runs it, so the handler's `probe_status=$1` arrived blank.
		// The assignment must stay FIRST in the trap string — anything before it clobbers `$?` — which
		// is what tokenRemoved below now falsifies.
		// `PROBE_STATUS=0` is pinned between the two anchors, not skipped over — but NOT because a
		// live path would otherwise read it unset. `exit "$PROBE_STATUS"` sits inside
		// `cleanup_probe_dir`, which nothing calls directly, and every trap string assigns before it
		// calls. The initialisation is what keeps that true under a later edit: a direct call, or one
		// more trap added without the assignment, would reach the exit with the variable unset, and
		// under `set -u` that aborts the cleanup it was supposed to complete. Pinning it keeps the
		// belt from being removed on the grounds that the braces are holding.
		pattern: `PROBE_DIR=.*PROBE_STATUS=0.*cleanup_probe_dir\(\).*exit\s+"\$PROBE_STATUS".*trap\s+'PROBE_STATUS=\$\?;\s*cleanup_probe_dir'\s+EXIT.*trap\s+'PROBE_STATUS=143;\s*cleanup_probe_dir'\s+TERM.*PROBE_DIR\s*=\s*\$\(mktemp\s+-d\s+"\$TMP_ROOT/boss-review-tier-a\.XXXXXX"\)`,
		live:    "PROBE_DIR=; PROBE_STATUS=0; cleanup_probe_dir() { exit \"$PROBE_STATUS\" }; trap 'PROBE_STATUS=$?; cleanup_probe_dir' EXIT; trap 'PROBE_STATUS=143; cleanup_probe_dir' TERM; PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-tier-a.XXXXXX\")",
		// One mutation per anchor, never a compound one: reordering the EXIT trap AND replacing
		// `exit "$PROBE_STATUS"` in the same string would prove only that the pattern rejects both
		// together, leaving either one free to stop being required.
		tokenRemoved: "PROBE_DIR=; PROBE_STATUS=0; cleanup_probe_dir() { exit \"$PROBE_STATUS\" }; trap 'cleanup_probe_dir; PROBE_STATUS=$?' EXIT; trap 'PROBE_STATUS=143; cleanup_probe_dir' TERM; PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-tier-a.XXXXXX\")",
		alsoRemoved: []string{
			// the exit anchor alone: trap order left intact
			"PROBE_DIR=; PROBE_STATUS=0; cleanup_probe_dir() { exit 0 }; trap 'PROBE_STATUS=$?; cleanup_probe_dir' EXIT; trap 'PROBE_STATUS=143; cleanup_probe_dir' TERM; PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-tier-a.XXXXXX\")",
			// the initialisation alone, the belt the comment above refuses to remove
			"PROBE_DIR=; cleanup_probe_dir() { exit \"$PROBE_STATUS\" }; trap 'PROBE_STATUS=$?; cleanup_probe_dir' EXIT; trap 'PROBE_STATUS=143; cleanup_probe_dir' TERM; PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-tier-a.XXXXXX\")",
			// the TERM trap alone: a pin on EXIT says nothing about cancellation
			"PROBE_DIR=; PROBE_STATUS=0; cleanup_probe_dir() { exit \"$PROBE_STATUS\" }; trap 'PROBE_STATUS=$?; cleanup_probe_dir' EXIT; PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-tier-a.XXXXXX\")",
		},
	},
	{
		name: "tier-b-cleans-up-on-cancellation",
		// Tier B carries its own copy of the status-in-a-variable contract, and a pin on Tier A says
		// nothing about it. Same three parts, same reason: a slash-command invocation blanks a
		// positional in a published body, so `cleanup_probe $?` would have run with no argument.
		// `MUTATION_ACTIVE=0` leads, because it is the first anchor that is unique to Tier B —
		// without it the greedy scan would satisfy the rest from Tier A's recipe above.
		pattern:      `MUTATION_ACTIVE=0.*PROBE_STATUS=0.*cleanup_probe\(\).*exit\s+"\$PROBE_STATUS".*trap\s+'PROBE_STATUS=\$\?;\s*cleanup_probe'\s+EXIT.*trap\s+'PROBE_STATUS=143;\s*cleanup_probe'\s+TERM`,
		live:         "MUTATION_ACTIVE=0; PROBE_STATUS=0; cleanup_probe() { exit \"$PROBE_STATUS\" }; trap 'PROBE_STATUS=$?; cleanup_probe' EXIT; trap 'PROBE_STATUS=143; cleanup_probe' TERM",
		tokenRemoved: "MUTATION_ACTIVE=0; cleanup_probe() { exit \"$PROBE_STATUS\" }; trap 'PROBE_STATUS=$?; cleanup_probe' EXIT; trap 'PROBE_STATUS=143; cleanup_probe' TERM",
	},
	{
		name:         "tier-a-checkout-root-preserves-trailing-newline",
		pattern:      `CHECKOUT_ROOT_OUTPUT=\$\(git\s+-C\s+"\$CHECKOUT_ROOT_INPUT"\s+rev-parse\s+--show-toplevel\s+&&\s+printf\s+'?x'?\).*CHECKOUT_ROOT=\$\{CHECKOUT_ROOT_OUTPUT%x\}.*CHECKOUT_ROOT_OUTPUT=\$\(cd\s+"\$CHECKOUT_ROOT"\s+&&\s+printf\s+'?%sx'?\s+"\$PWD"\).*CHECKOUT_ROOT=\$\{CHECKOUT_ROOT_OUTPUT%x\}`,
		live:         "CHECKOUT_ROOT_OUTPUT=$(git -C \"$CHECKOUT_ROOT_INPUT\" rev-parse --show-toplevel && printf x); CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}; CHECKOUT_ROOT_OUTPUT=$(cd \"$CHECKOUT_ROOT\" && printf '%sx' \"$PWD\"); CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}",
		tokenRemoved: "CHECKOUT_ROOT=$(git -C \"$CHECKOUT_ROOT\" rev-parse --show-toplevel)",
	},
	{
		name:         "tier-b-rejects-filesystem-root-checkout",
		pattern:      `Tier\s+B.*CHECKOUT_ROOT=\$\{CHECKOUT_ROOT_OUTPUT%x\}.*if\s+\[\s+"\$CHECKOUT_ROOT"\s+=\s+/\s+\].*Tier\s+B\s+cannot\s+safely\s+contain\s+a\s+checkout\s+rooted\s+at\s+/`,
		live:         "Tier B canonicalizes CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}; if [ \"$CHECKOUT_ROOT\" = / ]; then Tier B cannot safely contain a checkout rooted at /",
		tokenRemoved: "Tier B accepts a checkout rooted at /",
	},
	{
		name:         "tier-b-target-must-be-regular-file",
		pattern:      `if\s+\[\s+!\s+-f\s+"\$PROBE_TARGET"\s+\].*Tier\s+B\s+probe\s+target\s+must\s+be\s+a\s+regular\s+file.*git\s+-C\s+"\$CHECKOUT_ROOT"\s+ls-files\s+--error-unmatch`,
		live:         "if [ ! -f \"$PROBE_TARGET\" ]; then Tier B probe target must be a regular file; fi; git -C \"$CHECKOUT_ROOT\" ls-files --error-unmatch",
		tokenRemoved: "git ls-files alone validates a regular file",
	},
	{
		name:         "tier-b-rejects-mounted-targets",
		pattern:      `PROBE_MOUNTPOINT=\$\(command\s+-v\s+mountpoint\).*probe_target_is_not_mount\(\).*"\$PROBE_MOUNTPOINT"\s+-q\s+--\s+"\$PROBE_TARGET".*PROBE_MOUNT_STATUS.*case\s+"\$PROBE_MOUNT_STATUS".*32\)\s+return\s+0.*\*\)\s+return\s+1.*validate_probe_target\(\).*\[\s+-f\s+"\$PROBE_TARGET"\s+\].*probe_target_is_not_mount.*validate_probe_target_for_removal\(\).*\[\s+-L\s+"\$PROBE_TARGET"\s+\].*\[\s+!\s+-e\s+"\$PROBE_TARGET"\s+\].*\[\s+-f\s+"\$PROBE_TARGET"\s+\].*probe_target_is_not_mount.*validate_probe_target_for_restore\(\).*\[\s+!\s+-e\s+"\$PROBE_TARGET"\s+\].*\[\s+!\s+-L\s+"\$PROBE_TARGET"\s+\].*Tier\s+B\s+probe\s+target\s+must\s+not\s+be\s+a\s+mount\s+point.*backup_probe_target\(\).*validate_probe_target.*restore_probe_backup\(\).*validate_probe_target_for_restore.*remove_mutated_probe_target\(\).*validate_probe_target_for_removal.*rm\s+-f\s+--\s+"\$PROBE_TARGET".*backup_probe_target\s+\|\|\s+exit\s+1.*validate_probe_target.*MUTATION_ACTIVE=1`,
		live:         "PROBE_MOUNTPOINT=$(command -v mountpoint); probe_target_is_not_mount() { \"$PROBE_MOUNTPOINT\" -q -- \"$PROBE_TARGET\"; PROBE_MOUNT_STATUS=$?; case \"$PROBE_MOUNT_STATUS\" in 32) return 0 ;; *) return 1 ;; esac; }; validate_probe_target() { [ -f \"$PROBE_TARGET\" ] || return 1; probe_target_is_not_mount; }; validate_probe_target_for_removal() { [ -L \"$PROBE_TARGET\" ] && return 0; [ ! -e \"$PROBE_TARGET\" ] && return 0; [ -f \"$PROBE_TARGET\" ] || return 1; probe_target_is_not_mount; }; validate_probe_target_for_restore() { [ ! -e \"$PROBE_TARGET\" ] && [ ! -L \"$PROBE_TARGET\" ]; }; Tier B probe target must not be a mount point. backup_probe_target() calls validate_probe_target; restore_probe_backup() calls validate_probe_target_for_restore; remove_mutated_probe_target() calls validate_probe_target_for_removal and rm -f -- \"$PROBE_TARGET\". backup_probe_target || exit 1; validate_probe_target before MUTATION_ACTIVE=1",
		tokenRemoved: "mountpoint is checked only before backup",
	},
	{
		name:         "tier-b-revalidates-parent-before-cleanup",
		pattern:      `probe_parent_identity.*PROBE_PARENT_ID.*validate_probe_parent.*before.*removal.*restore.*fails?\s+without\s+traversing\s+a\s+changed\s+parent`,
		live:         "probe_parent_identity captures PROBE_PARENT_ID; validate_probe_parent runs immediately before every removal and restore and fails without traversing a changed parent",
		tokenRemoved: "validate_probe_parent only before the mutation",
	},
	{
		name:         "tier-b-keeps-backup-active-through-verification",
		pattern:      `MUTATION_ACTIVE=0.*only\s+after.*exactness.*cleanliness.*green\s+rerun.*second\s+exactness.*checkout-cleanliness\s+verification.*immediately\s+after.*final\s+green.*content,\s+mode,\s+and\s+metadata.*Leave\s+\x60?MUTATION_ACTIVE.*active.*post-green.*differs`,
		live:         "Set MUTATION_ACTIVE=0 only after exactness, cleanliness, the green rerun, then a second exactness and checkout-cleanliness verification immediately after that final green gate; repeat content, mode, and metadata checks; leave MUTATION_ACTIVE active when the post-green check differs",
		tokenRemoved: "Set MUTATION_ACTIVE=0 immediately after restore",
	},
	{
		name:         "tier-a-artifacts-stay-under-private-root",
		pattern:      `all\s+Tier-A\s+manifests,\s+replacement\s+sources,\s+copied\s+package\s+trees,\s+scripts,\s+and\s+cleanup.*beneath\s+\x60?"\$PROBE_DIR"\x60?.*never\s+use\s+shared\s+\x60?\$TMPDIR/<name>\x60?\s+paths`,
		live:         "all Tier-A manifests, replacement sources, copied package trees, scripts, and cleanup stay beneath \"$PROBE_DIR\"; never use shared $TMPDIR/<name> paths",
		tokenRemoved: "all Tier-A manifests, replacement sources, copied package trees, and scripts stay beneath \"$PROBE_DIR\"",
	},
	{
		name:         "tier-a-shell-runs-from-private-scratch",
		pattern:      `filesystem\s+sandbox.*\$PROBE_DIR.*only\s+writable\s+path.*cd\s+"\$PROBE_DIR".*env\s+-i.*sh\s+"\$PROBE_DIR/script\.sh".*never\s+run\s+the\s+probe\s+directly\s+from\s+the\s+checkout\s+or\s+from\s+an\s+unsandboxed\s+host\s+shell`,
		live:         "filesystem sandbox has $PROBE_DIR as its only writable path; cd \"$PROBE_DIR\" && env -i sh \"$PROBE_DIR/script.sh\"; never run the probe directly from the checkout or from an unsandboxed host shell",
		tokenRemoved: "cd \"$PROBE_DIR\" && env -i sh \"$PROBE_DIR/script.sh\"; never run the probe directly from the checkout or from an unsandboxed host shell",
	},
	{
		name:         "tier-a-shell-requires-filesystem-confinement",
		pattern:      `absolute\s+path.*inherited\s+variable.*filesystem\s+sandbox.*writable\s+paths\s+are\s+limited\s+to\s+\x60?"\$PROBE_DIR"\x60?.*readable\s+view.*explicit\s+allowlist.*probe\s+root.*copied\s+fixture.*required\s+tool\s+binaries.*read-only\s+dependency\s+cache.*do\s+not\s+expose\s+host\s+credentials.*cleared\s+environment.*deny\s+writes\s+outside.*read\s+and\s+write\s+confinement.*is\s+unavailable.*not\s+a\s+zero-write\s+probe`,
		live:         "An absolute path or inherited variable requires a filesystem sandbox whose writable paths are limited to \"$PROBE_DIR\" and whose readable view is an explicit allowlist of the probe root, copied fixture, required tool binaries, and read-only dependency cache. Do not expose host credentials; use a cleared environment and deny writes outside. If read and write confinement is unavailable, it is not a zero-write probe",
		tokenRemoved: "An absolute path or inherited variable requires a filesystem sandbox whose writable paths are limited to \"$PROBE_DIR\", with a cleared environment and deny writes outside; if confinement is unavailable, it is not a zero-write probe",
	},
	{
		name:         "tier-a-shell-requires-network-isolation",
		pattern:      `network\s+access\s+is\s+disabled.*including\s+loopback.*link-local.*metadata.*network\s+isolation\s+is\s+unavailable.*reject\s+the\s+probe`,
		live:         "Network access is disabled, including loopback, link-local, and metadata endpoints. If network isolation is unavailable, reject the probe",
		tokenRemoved: "Network access is available when filesystem confinement succeeds",
	},
	{
		name:         "tier-a-shell-rejects-unisolated-dependencies",
		pattern:      `dependencies\s+cannot\s+be\s+copied\s+or\s+otherwise\s+isolated\s+beneath\s+\x60?"\$PROBE_DIR"\x60?.*reject\s+the\s+probe`,
		live:         "If dependencies cannot be copied or otherwise isolated beneath \"$PROBE_DIR\", reject the probe",
		tokenRemoved: "If dependencies cannot be copied beneath \"$PROBE_DIR\", retry the probe",
	},
	{
		name:         "go-overlay-non-writing-command",
		pattern:      `only\s+for\s+a\s+command\s+known\s+not\s+to\s+write\s+to\s+its\s+working\s+directory`,
		live:         "only for a command known not to write to its working directory",
		tokenRemoved: "for any command that writes to its working directory",
	},
	{
		name:         "whole-directory-copy",
		pattern:      `cp\s+-R.*whole\s+directory.*\$PROBE_DIR.*sibling\s+imports`,
		live:         "cp -R the whole directory to $PROBE_DIR so sibling imports resolve",
		tokenRemoved: "cp -R one file to $PROBE_DIR so imports resolve",
	},
	{
		name:         "whole-directory-copy-rejects-symlinks",
		pattern:      `before\s+copying.*reject.*symlink.*cp\s+-R.*retains\s+symlinks.*escape.*\$PROBE_DIR.*mutation.*original\s+target.*do\s+not\s+dereference\s+symlinks.*find\s+"\$SOURCE_VIEW"\s+-type\s+l\s+-print\s+>"\$SYMLINK_LIST".*if\s+\[\s+-s\s+"\$SYMLINK_LIST"\s+\].*exit\s+1.*cp\s+-R\s+"\$SOURCE_VIEW"\s+"\$PROBE_DIR/"`,
		live:         "Before copying, reject a symlink source tree: cp -R retains symlinks that escape $PROBE_DIR, so mutation can affect the original target. Do not dereference symlinks. find \"$SOURCE_VIEW\" -type l -print >\"$SYMLINK_LIST\"; if [ -s \"$SYMLINK_LIST\" ]; then exit 1; fi; cp -R \"$SOURCE_VIEW\" \"$PROBE_DIR/\"",
		tokenRemoved: "Before copying, copy a symlink source tree: cp -R retains symlinks that escape $PROBE_DIR",
	},
	{
		name:         "whole-directory-copy-preserves-checkout-atime",
		pattern:      `never\s+run.*find.*cp\s+-R.*against\s+the\s+checkout\s+source\s+directly.*atime`,
		live:         "Never run find or cp -R against the checkout source directly: either can update atime. SOURCE_VIEW is a read-only noatime view; reject the probe when it is unavailable. find \"$SOURCE_VIEW\" -type l -print >\"$SYMLINK_LIST\"; cp -R \"$SOURCE_VIEW\" \"$PROBE_DIR/\"",
		tokenRemoved: "Run cp -R against the checkout source directly without an atime-safe source view",
	},
	{
		name:         "whole-directory-copy-requires-noatime-source-view",
		pattern:      `SOURCE_VIEW.*read-only.*noatime.*reject\s+the\s+probe`,
		live:         "SOURCE_VIEW is a read-only noatime view; reject the probe when it is unavailable",
		tokenRemoved: "SOURCE_VIEW is a copied source tree",
	},
	{
		name:         "tier-b-linux-restores-target-atime",
		pattern:      `restore_probe_atime\(\).*touch\s+-a\s+-d\s+"\$PROBE_ATIME"`,
		live:         "PROBE_ATIME=$(stat -c '%x' \"$PROBE_TARGET\"); restore_probe_atime() runs touch -a -d \"$PROBE_ATIME\"; restore_probe_backup() runs PROBE_TAR then restore_probe_atime. Darwin exact access-time restore is unavailable; reject the probe",
		tokenRemoved: "restore_probe_backup() runs tar but leaves the restored target atime at restore time",
	},
	{
		name:         "tier-b-linux-captures-target-atime-and-rejects-unsupported-hosts",
		pattern:      `PROBE_ATIME=.*stat\s+-c\s+'%x'.*exact\s+access-time\s+restore.*unavailable.*Darwin.*reject\s+the\s+probe`,
		live:         "PROBE_ATIME=$(stat -c '%x' \"$PROBE_TARGET\"); exact access-time restore is unavailable on Darwin; reject the probe",
		tokenRemoved: "PROBE_ATIME is ignored when exact access-time restore is unavailable",
	},
	{
		name:         "interpreted-source-requires-filesystem-confinement",
		pattern:      `Interpreted\s+source.*same\s+cleared-environment\s+filesystem\s+sandbox.*explicit\s+readable\s+allowlist.*writable\s+paths\s+limited\s+to\s+\x60?"\$PROBE_DIR"\x60?.*no\s+host\s+credentials.*home\s+paths.*deny\s+writes\s+outside.*confinement\s+is\s+unavailable.*reject\s+the\s+probe`,
		live:         "Interpreted source runs in the same cleared-environment filesystem sandbox with an explicit readable allowlist, writable paths limited to \"$PROBE_DIR\", no host credentials or home paths, and deny writes outside; if confinement is unavailable, reject the probe",
		tokenRemoved: "Interpreted source runs from the copied directory without a filesystem sandbox",
	},
	{
		name:         "literal-shell-script",
		pattern:      `literal\s+shell\s+line.*\$PROBE_DIR/script\.sh.*inline\s+sh\s+-c`,
		live:         "put the literal shell line in a $PROBE_DIR/script.sh, never use inline sh -c",
		tokenRemoved: "put the shell line in a $TMPDIR script and run it",
	},
	{
		name:         "pre-fix-git-show",
		pattern:      `git\s+show\s+HEAD:<path>.*pre-fix\s+version`,
		live:         "git show HEAD:<path> selects the pre-fix version",
		tokenRemoved: "git show selects the pre-fix version",
	},
	{
		name:         "pre-fix-git-show-explicit-revision",
		pattern:      `otherwise\s+use\s+git\s+show\s+<pre-fix\s+revision>:<path>`,
		live:         "otherwise use git show <pre-fix revision>:<path>",
		tokenRemoved: "otherwise use git show HEAD:<path>",
	},
	{
		name:         "commit-first",
		pattern:      `Commit\s+the\s+work\s+first`,
		live:         "Commit the work first",
		tokenRemoved: "Save the work first",
	},
	{
		name:         "private-probe-directory",
		pattern:      `TMP_ROOT_OUTPUT\s*=\s*\$\(cd\s+-P\s+"\$TMP_ROOT".*printf\s+'?%sx'?\s+"\$PWD"\).*TMP_ROOT=\$\{TMP_ROOT_OUTPUT%x\}.*Tier\s+B\s+temporary\s+root\s+is\s+inside\s+the\s+checkout.*PROBE_DIR\s*=\s*\$\(mktemp\s+-d\s+"\$TMP_ROOT/boss-review-probe\.XXXXXX"\).*Tier\s+B\s+backup\s+directory\s+is\s+inside\s+the\s+checkout`,
		live:         "TMP_ROOT_OUTPUT=$(cd -P \"$TMP_ROOT\" && printf '%sx' \"$PWD\"); TMP_ROOT=${TMP_ROOT_OUTPUT%x}; Tier B temporary root is inside the checkout; PROBE_DIR=$(mktemp -d \"$TMP_ROOT/boss-review-probe.XXXXXX\"); Tier B backup directory is inside the checkout",
		tokenRemoved: "PROBE_DIR=$(mktemp -d \"${TMPDIR:-/tmp}/boss-review-probe.XXXXXX\")",
	},
	{
		name:         "private-backup-copy-restore",
		pattern:      "metadata-preserving\\s+backup.*remove\\s+the\\s+mutated\\s+destination.*prevents\\s+the\\s+restore\\s+tool\\s+from\\s+following\\s+a\\s+symlink",
		live:         "use a metadata-preserving backup; remove the mutated destination before restoring prevents the restore tool from following a symlink",
		tokenRemoved: "cp -p \"<abs path>\" \"$PROBE_DIR/probe.bak\", then cp -p \"$PROBE_DIR/probe.bak\" \"<abs path>\" dereferences symlinks",
	},
	{
		name:         "private-backup-preserves-extended-metadata",
		pattern:      `cp\s+-pP.*not\s+an\s+exact\s+backup.*extended\s+attributes.*ACL.*security\s+context.*metadata-preserving\s+backup.*capability\s+is\s+unavailable.*reject\s+the\s+probe`,
		live:         "cp -pP is not an exact backup of extended attributes, ACLs, or security contexts; use a metadata-preserving backup, and if that capability is unavailable reject the probe",
		tokenRemoved: "cp -pP preserves all extended metadata everywhere",
	},
	{
		name:         "private-backup-trap-restores-before-cleanup",
		pattern:      `cleanup_probe\(\).*MUTATION_ACTIVE.*remove_mutated_probe_target.*restore_probe_backup.*rm\s+-rf\s+--\s+"\$PROBE_DIR"`,
		live:         "cleanup_probe() checks MUTATION_ACTIVE, then remove_mutated_probe_target and restore_probe_backup before rm -rf -- \"$PROBE_DIR\"",
		tokenRemoved: "cleanup_probe() checks MUTATION_ACTIVE, then rm -rf -- \"$PROBE_DIR\"",
	},
	{
		name: "private-backup-trap-restores-on-posix-signals",
		// Same PROBE_STATUS reshape as tier A, and the same ordering constraint: tokenRemoved keeps all
		// four traps and moves only the assignment behind the call, which is the one mutation that
		// still looks right and silently reports 0 for every signal.
		pattern:      `trap\s+'PROBE_STATUS=\$\?;\s*cleanup_probe'\s+EXIT.*trap\s+'PROBE_STATUS=129;\s*cleanup_probe'\s+HUP.*trap\s+'PROBE_STATUS=130;\s*cleanup_probe'\s+INT.*trap\s+'PROBE_STATUS=143;\s*cleanup_probe'\s+TERM.*MUTATION_ACTIVE=1`,
		live:         "trap 'PROBE_STATUS=$?; cleanup_probe' EXIT; trap 'PROBE_STATUS=129; cleanup_probe' HUP; trap 'PROBE_STATUS=130; cleanup_probe' INT; trap 'PROBE_STATUS=143; cleanup_probe' TERM; MUTATION_ACTIVE=1",
		tokenRemoved: "trap 'cleanup_probe; PROBE_STATUS=$?' EXIT; trap 'PROBE_STATUS=129; cleanup_probe' HUP; trap 'PROBE_STATUS=130; cleanup_probe' INT; trap 'PROBE_STATUS=143; cleanup_probe' TERM; MUTATION_ACTIVE=1",
	},
	{
		name:         "private-probe-exact-cleanup",
		pattern:      `rm\s+-rf\s+--\s+"\$PROBE_DIR"`,
		live:         "rm -rf -- \"$PROBE_DIR\"",
		tokenRemoved: "rm -rf \"$TMPDIR\"",
	},
	{
		name:         "checkout-destroys-work",
		pattern:      `git\s+checkout\s+--\s+<file>.*discards\s+uncommitted\s+edits`,
		live:         "git checkout -- <file> discards uncommitted edits",
		tokenRemoved: "git checkout -- <file> restores edits",
	},
	{
		name:         "absolute-no-inherited-cwd",
		pattern:      `absolute\s+paths.*never\s+inherit\s+the\s+cwd`,
		live:         "use absolute paths and never inherit the cwd",
		tokenRemoved: "use paths and inherit the cwd",
	},
	{
		name:         "exact-delete-no-glob",
		pattern:      `Delete\s+scratch\s+files\s+by\s+exact\s+path,\s+never\s+by\s+glob`,
		live:         "Delete scratch files by exact path, never by glob",
		tokenRemoved: "Delete scratch files when done",
	},
	{
		name:         "input-emptiness",
		pattern:      `what\s+would\s+make\s+this\s+check's\s+input\s+set\s+go\s+empty`,
		live:         "what would make this check's input set go empty",
		tokenRemoved: "what would change this check's input set",
	},
	{
		name:         "narrowing-tripwire",
		pattern:      `derivation.*widening\s+direction\s+only.*pinned\s+list.*narrowing\s+tripwire.*both\s+directions`,
		live:         "derivation covers the widening direction only; keep a pinned list as a narrowing tripwire and compare both directions",
		tokenRemoved: "derivation covers widening; keep a list and compare it",
	},
}...))

var falsificationCitationPin = regProsePin(falsificationProsePin{
	name:         "falsification-reference",
	pattern:      `Use\s+references/falsification\.md\s+for\s+the\s+probe`,
	live:         "Use references/falsification.md for the probe",
	tokenRemoved: "Use references/non-vacuity.md for the probe",
})

var falsificationResolvedCitationPin = regProsePin(falsificationProsePin{
	name:         "falsification-resolved-subagent-reference",
	pattern:      `<FALSIFICATION_REFERENCE>.*resolved\s+absolute\s+installed\s+path`,
	live:         "Read <FALSIFICATION_REFERENCE>, a resolved absolute installed path, before the probe",
	tokenRemoved: "Read $BOSS_SKILLS_HOME/boss-review/references/falsification.md before the probe",
})

var falsificationTierAOnlyPin = regProsePin(falsificationProsePin{
	name:         "falsification-tier-a-only",
	pattern:      `<FALSIFICATION_REFERENCE>.*Tier\s+A\s+only`,
	live:         "<FALSIFICATION_REFERENCE> — then use Tier A only",
	tokenRemoved: "<FALSIFICATION_REFERENCE> — then use Tier B only",
})

var falsificationTierBAfterCommitPin = regProsePin(falsificationProsePin{
	name:         "falsification-tier-b-after-commit",
	pattern:      `<FALSIFICATION_REFERENCE>.*Follow\s+Tier\s+B\s+after\s+committing\s+the\s+work`,
	live:         "<FALSIFICATION_REFERENCE> for the probe. Follow Tier B after committing the work",
	tokenRemoved: "<FALSIFICATION_REFERENCE> for the probe. Follow Tier A before committing the work",
})

var falsificationTierOneEnvelopePin = regProsePin(falsificationProsePin{
	name:         "falsification-tier-one-envelope-reference",
	pattern:      `"falsificationReference"\s*:\s*"<FALSIFICATION_REFERENCE>"`,
	live:         `"falsificationReference": "<FALSIFICATION_REFERENCE>"`,
	tokenRemoved: `"falsificationReference": "references/falsification.md"`,
})

var falsificationTierOneHandoffPin = regProsePin(falsificationProsePin{
	name:         "falsification-tier-one-nested-reviewer-handoff",
	pattern:      `context\.falsificationReference.*nested\s+reviewer.*same\s+Tier-A-only\s+rule`,
	live:         "context.falsificationReference must reach a nested reviewer under the same Tier-A-only rule",
	tokenRemoved: "context.falsificationReference may be omitted for nested reviewers",
})

var falsificationHostNativeHandoffPin = regProsePin(falsificationProsePin{
	name:         "falsification-host-native-round-handoff",
	pattern:      "Pass\\s+it\\s+`?<FALSIFICATION_REFERENCE>`?.*read\\s+that\\s+recipe\\s+and\\s+use\\s+Tier\\s+A\\s+only",
	live:         "Pass it `<FALSIFICATION_REFERENCE>`, the resolved absolute installed path from Phase 0, and require it to read that recipe and use Tier A only",
	tokenRemoved: "Pass it `<FALSIFICATION_REFERENCE>`, the resolved absolute installed path from Phase 0, and require it to review the diff",
})

var falsificationAcceptanceMutationPin = regProsePin(falsificationProsePin{
	name:         "falsification-acceptance-production-feed-mutation",
	pattern:      `require\s+evidence\s+that\s+the\s+named\s+property\s+was\s+killed\s+by\s+its\s+production-feed\s+mutation`,
	live:         "require evidence that the named property was killed by its production-feed mutation",
	tokenRemoved: "require evidence that the named property was reasoned from its literal",
})

var bossRepairZeroWriteBeforeCommitPin = regProsePin(falsificationProsePin{
	name:         "boss-repair-zero-write-before-commit",
	pattern:      `checklist\s+runs\s+before\s+commit,\s+use\s+only\s+a\s+zero-write\s+scratch\s+copy\.\s+Do\s+not\s+mutate\s+the\s+checkout`,
	live:         "checklist runs before commit, use only a zero-write scratch copy. Do not mutate the checkout",
	tokenRemoved: "checklist runs before commit, mutate the checkout when necessary",
})

var bossRepairScratchMutationPin = regProsePin(falsificationProsePin{
	name:         "boss-repair-scratch-mutation",
	pattern:      `Mutate\s+the\s+production\s+feed,\s+never\s+the\s+assertion\*+,\s+using\s+the\s+zero-write\s+scratch\s+copy`,
	live:         "**Mutate the production feed, never the assertion**, using the zero-write scratch copy",
	tokenRemoved: "**Mutate the production feed, never the assertion**, using a scratch copy when possible",
})

var bossRepairScratchConfinementPin = regProsePin(falsificationProsePin{
	name:         "boss-repair-scratch-confinement",
	pattern:      `shell,\s+interpreted-source,\s+or\s+test-gate\s+invocation.*filesystem\s+sandbox.*only\s+writable\s+path.*\x60?"\$PROBE_DIR"\x60?.*explicit\s+read-only\s+allowlist.*cleared\s+environment.*deny\s+writes\s+outside.*network.*disabled.*loopback.*link-local.*metadata.*filesystem\s+or\s+network\s+confinement\s+is\s+unavailable.*reject\s+the\s+probe`,
	live:         "A shell, interpreted-source, or test-gate invocation needs a filesystem sandbox whose only writable path is \"$PROBE_DIR\", an explicit read-only allowlist, a cleared environment, and must deny writes outside. Network access is disabled, including loopback, link-local, and metadata endpoints. If filesystem or network confinement is unavailable, reject the probe",
	tokenRemoved: "A shell, interpreted-source, or test-gate invocation can run from a scratch copy without filesystem or network confinement",
})

var coreMethodologyTierPin = regProsePin(falsificationProsePin{
	name:         "core-methodology-tier-a-only",
	pattern:      `read-only\s+reviewer.*Tier\s+A\s+only`,
	live:         "A read-only reviewer may use Tier A only",
	tokenRemoved: "A read-only reviewer may use either tier",
})

// bossReviewClaimVerificationPins pins the six rules that require a claim to be VERIFIED before it
// is written into prose or a comment. Each rule's teeth is one checkable action -- a grep, a proof
// against the callee, an enumeration -- and the sentences around it are restatable motivation. So
// each pin sits on the action, and each tokenRemoved fixture kills precisely the token that makes
// the action checkable rather than merely rewording the rule: "grep the symbol" without "paste the
// result" still reads as a rule and licenses an unpasted guess, which is the shape that shipped the
// false no-caller comment these rules exist to prevent.
var bossReviewClaimVerificationPins = regProsePins([]falsificationProsePin{
	{
		name:         "claim-grep-repo-wide-fact",
		pattern:      `grep\s+the\s+symbol\s+and\s+paste\s+the\s+result`,
		live:         "grep the symbol and paste the result",
		tokenRemoved: "grep the symbol and move on",
	},
	{
		name:         "claim-equivalence-against-callee",
		pattern:      `prove\s+it\s+against\s+the\s+callee's\s+actual\s+argument\s+handling`,
		live:         "prove it against the callee's actual argument handling",
		tokenRemoved: "prove it against the callee's signature",
	},
	{
		name:         "claim-subtotal-states-total",
		pattern:      `must\s+also\s+state\s+the\s+total\s+it\s+partitions`,
		live:         "must also state the total it partitions",
		tokenRemoved: "must also state its derivation",
	},
	{
		name:         "claim-list-ratchet-covers-lists-only",
		pattern:      `separate\s+reading\s+pass,\s+or\s+a\s+claim-level\s+assertion,\s+over\s+the\s+rationale\s+prose`,
		live:         "separate reading pass, or a claim-level assertion, over the rationale prose",
		tokenRemoved: "separate reading pass over the lists themselves",
	},
	{
		// The pin sits on "superseded" rather than on the breadth of the grep: a rule that keeps
		// "the whole documentation and skills trees" but names the CORRECTED term still reads as a
		// completeness rule while returning only the sites already fixed -- a green grep over the
		// exact set that needed no fixing.
		name:         "claim-grep-whole-tree-for-superseded-term",
		pattern:      `grep\s+the\s+whole\s+documentation\s+and\s+skills\s+trees\s+for\s+the\s+\*\*superseded\*\*\s+wording`,
		live:         "grep the whole documentation and skills trees for the **superseded** wording",
		tokenRemoved: "grep the whole documentation and skills trees for the corrected wording",
	},
	{
		name:         "claim-scope-self-referential-universal",
		pattern:      `enumerate\s+every\s+element\s+it\s+quantifies\s+over`,
		live:         "enumerate every element it quantifies over",
		tokenRemoved: "enumerate the elements nearby",
	},
})

// TestFalsificationProsePinsAreEnrolledStructurally is the tripwire on that registry. A pin
// declared as a plain composite literal is still asserted against real prose by
// assertFalsificationPins, so it goes green while its own pattern/live/tokenRemoved agreement is
// never checked — precisely the shape that let falsificationHostNativeHandoffPin ship a `live`
// fixture that could not match its own pattern. Enrolment through regProsePin/regProsePins is
// therefore mandatory, and this gate reads the package's own sources to enforce it rather than
// trusting the convention to be remembered. Grepping the source is the only available check: Go
// offers no hook that fires on a composite literal nobody passed to a registrar.
func TestFalsificationProsePinsAreEnrolledStructurally(t *testing.T) {
	// falsificationStepPins is cloned wholesale into falsificationReferencePins, which IS
	// registered; enrolling it too would run every step pin twice under a duplicate subtest name.
	exempt := map[string]string{
		"falsificationStepPins": "cloned wholesale into falsificationReferencePins, which is registered",
	}
	// Only the first line of a declaration is inspected, which is where every form this package
	// uses puts the type name: `= falsificationProsePin{`, `= []falsificationProsePin{`, and
	// `= append(slicesClone(...), []falsificationProsePin{`.
	decl := regexp.MustCompile(`(?m)^var\s+(\w+)\s*=\s*(.*)$`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	registered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(src), -1) {
			name, rhs := m[1], m[2]
			if !strings.Contains(rhs, "falsificationProsePin{") {
				continue
			}
			if strings.Contains(rhs, "regProsePin") {
				registered++
				continue
			}
			if why, ok := exempt[name]; ok {
				t.Logf("%s: %s is exempt (%s)", entry.Name(), name, why)
				continue
			}
			t.Errorf("%s declares %s as a bare falsificationProsePin literal — wrap it in regProsePin/regProsePins so TestFalsificationProsePinsAreNonVacuous ranges over it; an unenrolled pin asserts against real prose while its own fixtures go unchecked", entry.Name(), name)
		}
	}
	// A source scan that matched nothing would pass while enforcing nothing.
	if registered < 3 {
		t.Fatalf("found only %d registered pin declaration(s) by source scan — the declaration regexp no longer matches this package's pin vars, so this gate is inert", registered)
	}
	if len(allProsePins) < registered {
		t.Fatalf("registry holds %d pins but %d declarations call regProsePin — a registrar is being called without its result reaching allProsePins", len(allProsePins), registered)
	}
}

func TestFalsificationProsePinsAreNonVacuous(t *testing.T) {
	pins := slicesClone(allProsePins)
	if len(pins) == 0 {
		t.Fatal("the prose-pin registry is empty — every pin must be declared through regProsePin/regProsePins, and a harness with nothing to range over passes vacuously")
	}
	for _, pin := range pins {
		pin := pin
		t.Run(pin.name, func(t *testing.T) {
			if strings.Contains(pin.pattern, " ") {
				t.Fatalf("pattern contains a literal space instead of \\s+: %q", pin.pattern)
			}
			re := regexp.MustCompile(`(?is)` + pin.pattern)
			cases := []struct {
				name      string
				input     string
				wantMatch bool
			}{
				{name: "live-match", input: pin.live, wantMatch: true},
				{name: "line-wrapped-match", input: wrapProsePin(t, pin.live), wantMatch: true},
				{name: "token-removed-no-match", input: pin.tokenRemoved, wantMatch: false},
			}
			for i, mutated := range pin.alsoRemoved {
				cases = append(cases, struct {
					name      string
					input     string
					wantMatch bool
				}{name: fmt.Sprintf("also-removed-%d-no-match", i), input: mutated, wantMatch: false})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if got := re.MatchString(tc.input); got != tc.wantMatch {
						t.Fatalf("pattern %q match=%v, want %v for %q", pin.pattern, got, tc.wantMatch, tc.input)
					}
				})
			}
		})
	}
}

func TestBossReviewFalsificationRecipeShipped(t *testing.T) {
	const referencePath = "skills/boss-review/references/falsification.md"
	const skillPath = "skills/boss-review/SKILL.md"
	const methodologyPath = "skills/boss-review/references/core-methodology.md"

	for payloadName, payload := range shippedPayloads(t) {
		payloadName, payload := payloadName, payload
		t.Run(payloadName, func(t *testing.T) {
			reference := readPayloadFile(t, payload, referencePath)
			if strings.TrimSpace(reference) == "" {
				t.Fatalf("%s is empty", referencePath)
			}
			assertContains(t, reference, `CHECKOUT_ROOT_INPUT=${BOSS_REVIEW_CHECKOUT_ROOT:-}
if [ -n "$CHECKOUT_ROOT_INPUT" ]; then
  # Keep a trailing newline in the checkout path: command substitution strips it.
  CHECKOUT_ROOT_OUTPUT=$(git -C "$CHECKOUT_ROOT_INPUT" rev-parse --show-toplevel && printf x) || exit 1
else
  CHECKOUT_ROOT_OUTPUT=$(git rev-parse --show-toplevel && printf x) || exit 1
fi
# Git ends --show-toplevel with a record newline. Remove that delimiter and the
# sentinel together, leaving any newline that belongs to the checkout path.
CHECKOUT_ROOT_RECORD_SUFFIX=$(printf '\nx')
CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%"$CHECKOUT_ROOT_RECORD_SUFFIX"}
CHECKOUT_ROOT_OUTPUT=$(cd "$CHECKOUT_ROOT" && printf '%sx' "$PWD") || exit 1
CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}`)
			assertContains(t, reference, `TMP_ROOT_OUTPUT=$(cd "$TMP_ROOT" && printf '%sx' "$PWD") || exit 1
TMP_ROOT=${TMP_ROOT_OUTPUT%x}`)
			assertContains(t, reference, `   TMP_ROOT_OUTPUT=$(cd -P "$TMP_ROOT" && printf '%sx' "$PWD") || exit 1
   TMP_ROOT=${TMP_ROOT_OUTPUT%x}
   case "$TMP_ROOT" in
     "$CHECKOUT_ROOT"|"$CHECKOUT_ROOT"/*)
       printf '%s\n' "Tier B temporary root is inside the checkout" >&2
       exit 1
       ;;
   esac
   PROBE_TARGET="<absolute path>"`)
			if strings.Contains(reference, "-quit") {
				t.Fatalf("%s must not use GNU find -quit", referencePath)
			}
			assertContains(t, reference, `  SYMLINK_LIST="$PROBE_DIR/symlinks"
  if ! find "$SOURCE_VIEW" -type l -print >"$SYMLINK_LIST"; then
    echo "Tier A could not inspect source tree for symlinks" >&2
    exit 1
  fi
  if [ -s "$SYMLINK_LIST" ]; then
    echo "Tier A cannot copy a source tree containing symlinks" >&2
    exit 1
  fi`)
			if strings.Contains(reference, "$TMPDIR/probe.bak") {
				t.Fatalf("%s must not use the shared $TMPDIR/probe.bak scratch path", referencePath)
			}
			assertContains(t, reference, `PROBE_BASENAME=${PROBE_TARGET##*/}`)
			assertContains(t, reference, `PROBE_PARENT_INPUT=${PROBE_TARGET%/*}`)
			assertContains(t, reference, `PROBE_PARENT_LOGICAL=$(cd -L "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD")`)
			assertContains(t, reference, `PROBE_PARENT_LOGICAL=${PROBE_PARENT_LOGICAL%x}`)
			assertContains(t, reference, `PROBE_PARENT=$(cd -P "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD")`)
			assertContains(t, reference, `PROBE_PARENT=${PROBE_PARENT%x}`)
			assertContains(t, reference, `PROBE_TARGET="$PROBE_PARENT/$PROBE_BASENAME"`)
			assertContains(t, reference, `--atime-preserve=system`)
			assertContains(t, reference, `--same-permissions`)
			assertContains(t, reference, `PROBE_MODE=$(stat -c '%a' "$PROBE_TARGET")`)
			assertContains(t, reference, `"$PROBE_DIR/probe.tar" -C "$PROBE_PARENT" "./$PROBE_BASENAME"`)
			assertNotContains(t, reference, `$(basename "$PROBE_TARGET")`)
			assertNotContains(t, reference, `$(dirname "$PROBE_TARGET")`)
			assertFalsificationPins(t, reference, falsificationReferencePins)
			assertPinsInOrder(t, reference, falsificationStepPins)

			skill := readPayloadFile(t, payload, skillPath)
			citationWindows := map[string]string{
				"phase-0":                  sectionBetween(t, skill, "## Phase 0 — Setup", "## Phase 1"),
				"acceptance-certification": sectionBetween(t, skill, "## Acceptance-criteria certification", "## Phase 0"),
				"phase-1-extension":        sectionBetween(t, skill, "### Tier 1 — a discovered lens extension bound to this lens id", "### Tier 2 — the lens entry's `skill`"),
				"phase-1-reviewer":         sectionBetween(t, skill, "### Tier 2 — the lens entry's `skill`", "### Tier 3 — the lens entry's inline `fallbackRubric`"),
				"phase-r-extension":        sectionBetween(t, skill, "### Tier 1 — repo-local round extensions", "### Tier 2 — host-native whole-diff review"),
				"phase-r-native":           sectionBetween(t, skill, "### Tier 2 — host-native whole-diff review", "### Tier 3 — inline whole-diff rubric"),
				// Phase D (the opportunistic default rounds) now sits between Phase R's last tier and
				// Phase 5, so the window ends at Phase D rather than Phase 5 — sectionBetween refuses a
				// window that would silently span an intervening section.
				"phase-r-inline": sectionBetween(t, skill, "### Tier 3 — inline whole-diff rubric", "## Phase D"),
				"phase-d":        sectionBetween(t, skill, "## Phase D — Default rounds", "## Phase 5"),
				"phase-6-fix":    sectionBetween(t, skill, "## Phase 6 — Fix must-fix", "## Phase 7"),
			}

			// Phase D is opportunistic, so every property that keeps it from degrading the
			// guaranteed review lives in prose alone — nothing downstream fails when one is
			// dropped. Pin the ones that are load-bearing rather than descriptive.
			//
			// The leg-list price. Phase D's findings join the ordinary must-fix set, so gating
			// its admission on the dispatch leg alone can commit the run to a Phase 6 round it
			// cannot fund — an optional add-on that forces a capped report.
			assertContains(t, skill, "- **Phase D** — the opportunistic default-round batch → `DEADLINE_LEG_SECONDS + FIX_ROUND_SECONDS`.")
			assertContains(t, citationWindows["phase-d"], "`LEG_SECONDS=$(( DEADLINE_LEG_SECONDS + FIX_ROUND_SECONDS ))`")
			// Additive, never a tier: without this, an all-skipped Phase D could be read as
			// licence to treat a default round as a substitute for Phase R's own fallbacks.
			assertContains(t, citationWindows["phase-d"], "It never suppresses Phase R's Tier 2 or Tier 3")
			// The operator escape hatch, which needs no config edit.
			assertContains(t, citationWindows["phase-d"], "When `BOSS_REVIEW_DEFAULT_ROUNDS=0`, skip this entire phase")
			// Termination: a second voice that re-opens findings each round turns a converging
			// fix loop into one bounded only by $MAX_ROUNDS.
			assertContains(t, citationWindows["phase-6-fix"], "**Phase D MUST NOT re-run on a confirming pass**")
			// The whole point of a cross-agent round is that the judgement happens in the OTHER
			// model. A worker that reads the diff itself still emits a well-formed envelope
			// under the second-voice lens, so the failure is a same-model round wearing the
			// label — undetectable downstream, and pinned only here.
			assertContains(t, citationWindows["phase-d"], "the worker does **not** review the branch itself")
			assertContains(t, citationWindows["phase-d"], "same-model round wearing the label**")
			assertContains(t, citationWindows["phase-d"], `node "<TOOLBOX>/<SECOND_VOICE>-review.mjs" run`)
			assertContains(t, citationWindows["phase-d"], "never a finding and never a fallback to")
			// A worker is a fresh subagent with a fresh shell. Shipping the dispatch with live
			// `$VAR` references instead of substituted placeholders degrades the round from
			// same-model to always-skipped — inert either way, and equally silent.
			assertContains(t, citationWindows["phase-d"], "a worker inherits no shell variable from here")
			// The helper returns prose; triage requires a non-blank `file` and a severity from a
			// closed vocabulary. Without the schema, unmappable items land in `invalid`, which
			// denies the run a clean verdict for the sin of having consulted a second model.
			assertContains(t, citationWindows["phase-d"], `{ "severity": "Critical|Warning|Suggestion", "file": "<path>", "line": <int|null>,`)
			for _, site := range []string{"acceptance-certification"} {
				window := citationWindows[site]
				t.Run(site, func(t *testing.T) {
					assertFalsificationPins(t, window, []falsificationProsePin{falsificationCitationPin})
				})
			}
			assertFalsificationPins(t, citationWindows["phase-1-reviewer"], []falsificationProsePin{falsificationTierAOnlyPin})
			assertFalsificationPins(t, citationWindows["phase-r-native"], []falsificationProsePin{falsificationHostNativeHandoffPin})
			assertFalsificationPins(t, citationWindows["phase-r-inline"], []falsificationProsePin{falsificationTierAOnlyPin})
			assertFalsificationPins(t, citationWindows["phase-6-fix"], []falsificationProsePin{falsificationTierBAfterCommitPin})
			for _, site := range []string{"phase-1-extension", "phase-r-extension"} {
				assertFalsificationPins(t, citationWindows[site], []falsificationProsePin{falsificationTierOneEnvelopePin, falsificationTierOneHandoffPin})
			}
			const falsificationResolver = `BOSS_REVIEW_FALSIFICATION_REFERENCE="$(cd "$BOSS_SKILLS_HOME/boss-review/references" && pwd)/falsification.md"
test -f "$BOSS_REVIEW_FALSIFICATION_REFERENCE" || { echo "BLOCKED: installed boss-review falsification reference not found"; exit 1; }`
			assertContains(t, citationWindows["phase-0"], falsificationResolver)
			for _, site := range []string{"phase-1-reviewer", "phase-r-inline", "phase-6-fix"} {
				assertFalsificationPins(t, citationWindows[site], []falsificationProsePin{falsificationResolvedCitationPin})
				assertNotContains(t, citationWindows[site], "$BOSS_SKILLS_HOME/boss-review/references/falsification.md")
			}
			assertFalsificationPins(t, citationWindows["acceptance-certification"], []falsificationProsePin{falsificationAcceptanceMutationPin})

			methodology := readPayloadFile(t, payload, methodologyPath)
			reviewerSplit := sectionBetween(t, methodology, "## Reviewer / orchestrator split", "## Findings contract")
			assertFalsificationPins(t, reviewerSplit, []falsificationProsePin{falsificationCitationPin, coreMethodologyTierPin})
		})
	}
}

func TestTierBSignalTrapRestoresMutationUnderDash(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash is unavailable")
	}

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backup")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}

	script := `
set -eu
PROBE_TARGET=$1
PROBE_DIR=$2
MUTATION_ACTIVE=0
PROBE_STATUS=0
backup_probe_target() {
  cp -pP "$PROBE_TARGET" "$PROBE_DIR/probe.bak"
}
restore_probe_backup() {
  cp -pP "$PROBE_DIR/probe.bak" "$PROBE_TARGET"
}
cleanup_probe() {
  trap - EXIT HUP INT TERM
  if [ "${MUTATION_ACTIVE:-0}" = 1 ]; then
    rm -rf -- "$PROBE_TARGET"
    restore_probe_backup
  fi
  rm -rf -- "$PROBE_DIR"
  exit "$PROBE_STATUS"
}
trap 'PROBE_STATUS=$?; cleanup_probe' EXIT
trap 'PROBE_STATUS=129; cleanup_probe' HUP
trap 'PROBE_STATUS=130; cleanup_probe' INT
trap 'PROBE_STATUS=143; cleanup_probe' TERM
backup_probe_target
MUTATION_ACTIVE=1
printf 'corrupted\n' >"$PROBE_TARGET"
kill -TERM $$
`
	cmd := exec.Command(dash, "-c", script, "dash", target, backupDir)
	if err := cmd.Run(); err == nil {
		t.Fatal("dash probe unexpectedly exited cleanly after SIGTERM")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original\n" {
		t.Fatalf("SIGTERM left target mutated: got %q", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("signal cleanup did not remove backup directory: %v", err)
	}
}

func TestTierBExitTrapPropagatesFailureStatusUnderDash(t *testing.T) {
	// The signal test above exercises `kill -TERM $$`, which reaches `cleanup_probe` through a trap
	// whose status is a LITERAL (`PROBE_STATUS=143`). That proves nothing about the EXIT trap, whose
	// status is captured from `$?` — and `PROBE_STATUS=$?` being FIRST in the trap string is the one
	// ordering the pins assert but no execution test covered. Ordinary failure is also the common
	// case: a probe that restores its target but reports 0 is a probe that silently passes.
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash is unavailable")
	}

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backup")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}

	script := `
set -eu
PROBE_TARGET=$1
PROBE_DIR=$2
MUTATION_ACTIVE=0
PROBE_STATUS=0
backup_probe_target() {
  cp -pP "$PROBE_TARGET" "$PROBE_DIR/probe.bak"
}
restore_probe_backup() {
  cp -pP "$PROBE_DIR/probe.bak" "$PROBE_TARGET"
}
cleanup_probe() {
  trap - EXIT HUP INT TERM
  if [ "${MUTATION_ACTIVE:-0}" = 1 ]; then
    rm -rf -- "$PROBE_TARGET"
    restore_probe_backup
  fi
  rm -rf -- "$PROBE_DIR"
  exit "$PROBE_STATUS"
}
trap 'PROBE_STATUS=$?; cleanup_probe' EXIT
trap 'PROBE_STATUS=129; cleanup_probe' HUP
trap 'PROBE_STATUS=130; cleanup_probe' INT
trap 'PROBE_STATUS=143; cleanup_probe' TERM
backup_probe_target
MUTATION_ACTIVE=1
printf 'corrupted\n' >"$PROBE_TARGET"
exit 7
`
	cmd := exec.Command(dash, "-c", script, "dash", target, backupDir)
	err = cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("dash probe did not fail: err=%v", err)
	}
	// 7, not 0 and not 143: the EXIT trap must carry the script's OWN status through, which is only
	// true while `PROBE_STATUS=$?` runs before anything else in that trap string.
	if got := exitErr.ExitCode(); got != 7 {
		t.Fatalf("EXIT trap did not propagate the failing status: got %d, want 7", got)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original\n" {
		t.Fatalf("EXIT path left target mutated: got %q", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("EXIT cleanup did not remove backup directory: %v", err)
	}
}

func TestTierBTargetBasenamePreservesTrailingNewline(t *testing.T) {
	const script = `
PROBE_TARGET=$1
PROBE_PARENT=$2
PROBE_BASENAME=${PROBE_TARGET##*/}
PROBE_TARGET="$PROBE_PARENT/$PROBE_BASENAME"
printf %s "$PROBE_TARGET"
`
	target := "/checkout/foo\n"
	parent := "/checkout"
	cmd := exec.Command("sh", "-c", script, "sh", target, parent)
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := parent + "/foo\n"
	if string(got) != want {
		t.Fatalf("Tier B target lost trailing newline: got %q, want %q", got, want)
	}
}

func TestTierBCheckoutRootPreservesTrailingNewline(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout\n")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	const script = `
git() { printf %s "$CHECKOUT_ROOT_FROM_GIT"; }
CHECKOUT_ROOT_OUTPUT=$(git rev-parse --show-toplevel && printf x) || exit 1
CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}
CHECKOUT_ROOT_OUTPUT=$(cd "$CHECKOUT_ROOT" && printf '%sx' "$PWD") || exit 1
CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}
printf %s "$CHECKOUT_ROOT"
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "CHECKOUT_ROOT_FROM_GIT="+checkout)
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != checkout {
		t.Fatalf("Tier B checkout root lost trailing newline: got %q, want %q", got, checkout)
	}
}

func TestTierBTargetParentPreservesTrailingNewline(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "plain\n")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "file")
	const script = `
PROBE_TARGET=$1
PROBE_PARENT_INPUT=${PROBE_TARGET%/*}
if [ -z "$PROBE_PARENT_INPUT" ]; then PROBE_PARENT_INPUT=/; fi
PROBE_PARENT_LOGICAL=$(cd -L "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD") || exit 1
PROBE_PARENT_LOGICAL=${PROBE_PARENT_LOGICAL%x}
PROBE_PARENT=$(cd -P "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD") || exit 1
PROBE_PARENT=${PROBE_PARENT%x}
PROBE_BASENAME=${PROBE_TARGET##*/}
PROBE_TARGET="$PROBE_PARENT/$PROBE_BASENAME"
printf %s "$PROBE_TARGET"
`
	cmd := exec.Command("sh", "-c", script, "sh", target)
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), "/plain\n/file") {
		t.Fatalf("Tier B target lost trailing newline in parent: got %q", got)
	}
}

func TestBossRepairStrategyCarriesFalsificationSteps(t *testing.T) {
	for payloadName, skill := range bossRepairSkillPayloads(t) {
		payloadName, skill := payloadName, skill
		t.Run(payloadName, func(t *testing.T) {
			strategy := sectionBetween(t, skill, "#### Strategy C: Review Feedback", "### Phase 3: Verify and Monitor")
			const commitMarker = "   - Commit with reference to review feedback:"
			commitOffset := strings.Index(strategy, commitMarker)
			if commitOffset < 0 {
				t.Fatalf("Strategy C is missing %q", commitMarker)
			}
			preCommit := strategy[:commitOffset]
			assertFalsificationPins(t, preCommit, falsificationStepPins)
			assertPinsInOrder(t, preCommit, falsificationStepPins)
			assertFalsificationPins(t, preCommit, []falsificationProsePin{bossRepairZeroWriteBeforeCommitPin, bossRepairScratchMutationPin, bossRepairScratchConfinementPin})
			if strings.Contains(strategy, "boss-review/references/") {
				t.Fatal("Strategy C must not cite a cross-core boss-review reference")
			}
		})
	}
}

func readPayloadFile(t *testing.T, payload fs.FS, path string) string {
	t.Helper()
	contents, err := fs.ReadFile(payload, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

func assertFalsificationPins(t *testing.T, prose string, pins []falsificationProsePin) {
	t.Helper()
	for _, pin := range pins {
		t.Run(pin.name, func(t *testing.T) {
			if !regexp.MustCompile(`(?is)` + pin.pattern).MatchString(prose) {
				t.Errorf("prose does not match %q", pin.pattern)
			}
		})
	}
}

func assertPinsInOrder(t *testing.T, prose string, pins []falsificationProsePin) {
	t.Helper()
	remaining := prose
	for _, pin := range pins {
		match := regexp.MustCompile(`(?is)` + pin.pattern).FindStringIndex(remaining)
		if match == nil {
			t.Fatalf("ordered prose does not match %q", pin.pattern)
		}
		remaining = remaining[match[1]:]
	}
}

func wrapProsePin(t *testing.T, prose string) string {
	t.Helper()
	middle := len(prose) / 2
	before := strings.LastIndex(prose[:middle], " ")
	afterOffset := strings.Index(prose[middle:], " ")
	after := -1
	if afterOffset >= 0 {
		after = middle + afterOffset
	}
	index := before
	if index < 0 || (after >= 0 && middle-before > after-middle) {
		index = after
	}
	if index < 0 {
		t.Fatalf("fixture %q has no space to line-wrap", prose)
	}
	return prose[:index] + "\n" + prose[index+1:]
}

func slicesClone[T any](values []T) []T {
	return append([]T(nil), values...)
}

func TestBossReviewConfirmationSurfaceIncludesVerifiedFindings(t *testing.T) {
	skill := readEmbeddedBossReviewSkill(t)
	phase6 := sectionBetween(t, skill, "## Phase 6 — Fix must-fix", "## Phase 7 — Report")

	assertContains(t, phase6, "cited files of every `verified` must-fix item")
	assertContains(t, phase6, "the cited files still form the confirming surface; do not skip")

	methodologyBytes, err := SkillsFS.ReadFile("skills/boss-review/references/core-methodology.md")
	if err != nil {
		t.Fatalf("read boss-review methodology: %v", err)
	}
	methodology := string(methodologyBytes)
	assertContains(t, methodology, "cited files of every verified finding")
}

func TestBossReviewEmbeddedSkillCopiesStayIdentical(t *testing.T) {
	repoRoot := findRepoRoot(t)
	for _, rel := range []string{
		"SKILL.md",
		filepath.Join("references", "core-methodology.md"),
	} {
		t.Run(rel, func(t *testing.T) {
			embedded, err := SkillsFS.ReadFile("skills/boss-review/" + filepath.ToSlash(rel))
			if err != nil {
				t.Fatalf("read embedded boss-review %s: %v", rel, err)
			}
			mirror, err := os.ReadFile(filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-review", rel))
			if err != nil {
				t.Fatalf("read plugin boss-review %s: %v", rel, err)
			}
			if string(embedded) != string(mirror) {
				t.Errorf("boss-review %s differs between services/boss and bossd-plugin-claude; run `make copy-skills`", rel)
			}
		})
	}
}

func readEmbeddedBossReviewSkill(t *testing.T) string {
	t.Helper()

	skillBytes, err := SkillsFS.ReadFile("skills/boss-review/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded boss-review skill: %v", err)
	}
	return string(skillBytes)
}

// TestBossReviewSkillClaimVerificationRules asserts the claim-verification rules against the real
// payload. They are pure prose obligations: nothing downstream reds when one is dropped or softened
// into an exhortation, and the defect they prevent -- a confidently false statement in a comment --
// is only ever caught by a human reader. The window is Operating rules specifically, so a rule
// quietly relocated out of the section every reviewer reads fails here rather than passing as a
// move; sectionBetween refuses a window that would silently span an intervening section.
func TestBossReviewSkillClaimVerificationRules(t *testing.T) {
	const skillPath = "skills/boss-review/SKILL.md"

	for payloadName, payload := range shippedPayloads(t) {
		payloadName, payload := payloadName, payload
		t.Run(payloadName, func(t *testing.T) {
			skill := readPayloadFile(t, payload, skillPath)
			operatingRules := sectionBetween(t, skill, "## Operating rules", "## Caller deadline (wall-clock cap)")
			assertFalsificationPins(t, operatingRules, bossReviewClaimVerificationPins)
		})
	}
}

// bossReviewMustFixOverrunPins pins the §Caller deadline half of the bounded must-fix override.
// Every one of these claims is pure prose: the toolbox enforces the arithmetic, but nothing
// downstream reds when the prose that tells a run to CALL the toolbox is softened back into "stop
// when the allowance does not fit". That softening is exactly the regression this change removes --
// a located, fixable, unattempted must-fix converted into a deferred item with the clock named as
// the cause. So each pin sits on the token that makes the override checkable (a round COUNT, a
// re-derived remainder, a single spend, an enumerated cause list) rather than on its motivation,
// and each tokenRemoved fixture kills that token while leaving a sentence that still reads as a
// rule -- the shape a well-meaning edit actually produces.
var bossReviewMustFixOverrunPins = regProsePins([]falsificationProsePin{
	{
		// The bound is a COUNT. A seconds-valued restatement would re-open the units confusion
		// the FIX_ROUND_SECONDS suffix exists to close, one gate later.
		name:         "overrun-bound-is-one-round",
		pattern:      `MUSTFIX_OVERRUN_ROUNDS\s+=\s+1\s+#\s+the\s+ONE\s+extra\s+fix\s+round`,
		live:         "MUSTFIX_OVERRUN_ROUNDS  = 1   # the ONE extra fix round an open, UNATTEMPTED must-fix may buy past",
		tokenRemoved: "MUSTFIX_OVERRUN_ROUNDS  = 2   # the ONE extra fix round an open, UNATTEMPTED must-fix may buy past",
		alsoRemoved: []string{
			"MUSTFIX_OVERRUN_ROUNDS  = 1   # extra fix rounds an open, UNATTEMPTED must-fix may buy past",
		},
	},
	{
		name:         "overrun-seconds-derived-from-rounds",
		pattern:      `MUSTFIX_OVERRUN_SECONDS\s+=\s+MUSTFIX_OVERRUN_ROUNDS\s+\*\s+FIX_ROUND_SECONDS`,
		live:         "MUSTFIX_OVERRUN_SECONDS = MUSTFIX_OVERRUN_ROUNDS * FIX_ROUND_SECONDS",
		tokenRemoved: "MUSTFIX_OVERRUN_SECONDS = 1200",
	},
	{
		// A second seconds-valued BUDGET is the failure mode: two quantities to keep in sync, one
		// of which the gate never reads. The pin holds the seconds figure in reporting position.
		name:         "overrun-seconds-reported-never-tested",
		pattern:      `MUSTFIX_OVERRUN_SECONDS.\s+is\s+the\s+figure\s+the\s+run\s+\*\*reports\*\*,\s+never\s+one\s+it\s+tests`,
		live:         "`MUSTFIX_OVERRUN_SECONDS` is the figure the run **reports**, never one it tests",
		tokenRemoved: "`MUSTFIX_OVERRUN_SECONDS` is the figure the run **reports** and the one it tests",
	},
	{
		// "dispatched against" is the whole trigger. "cleared" inverts it: it would fire the
		// override for a finding two rounds already failed to clear, and never for a fresh one.
		name:         "override-requires-never-dispatched",
		pattern:      `\*\*no\s+fix\s+round\s+has\s+been\s+dispatched\s+against\*\*`,
		live:         "a must-fix finding is open that **no fix round has been dispatched against**, refusing it",
		tokenRemoved: "a must-fix finding is open that **no fix round has cleared**, refusing it",
	},
	{
		name:         "override-is-per-run-not-per-finding",
		pattern:      `one\s+extra\s+round\s+for\s+the\s+whole\s+run,\s+never\s+one\s+per\s+finding\s+and\s+never\s+one\s+per\s+round`,
		live:         "one extra round for the whole run, never one per finding and never one per round",
		tokenRemoved: "one extra round for each open finding",
	},
	{
		name:         "override-decided-by-the-toolbox",
		pattern:      `bs-review-caps\.mjs.\s+admit-fix-round`,
		live:         `node "$BOSS_REVIEW_TOOLBOX/bs-review-caps.mjs" admit-fix-round`,
		tokenRemoved: `node "$BOSS_REVIEW_TOOLBOX/bs-review-caps.mjs" rounds`,
	},
	{
		// The two caps bound different things. An override that reached the round cap would also
		// break resolveMaxRounds' lower-only clamp -- a session granting itself more rounds.
		name:         "round-cap-evaluated-first-never-overridden",
		pattern:      `The\s+round\s+cap\s+is\s+evaluated\s+first\s+and\s+is\s+never\s+overridden`,
		live:         "**The round cap is evaluated first and is never overridden.**",
		tokenRemoved: "**The round cap is evaluated first.**",
		alsoRemoved: []string{
			"**The round cap is evaluated last and is never overridden.**",
		},
	},
	{
		name:         "remaining-seconds-re-derived-at-the-boundary",
		pattern:      `is\s+re-derived\s+from\s+.date\s+\+%s.\s+at\s+this\s+boundary\*\*,\s+never\s+carried`,
		live:         "**`remainingSeconds` is re-derived from `date +%s` at this boundary**, never carried from the",
		tokenRemoved: "**`remainingSeconds` is reused from the previous round's reading**, never carried from the",
	},
	{
		// `null` and 0 are opposite answers: no deadline at all versus a deadline already blown.
		name:         "null-remaining-is-never-a-zero-deadline",
		pattern:      `it\s+is\s+never\s+a\s+deadline\s+of\s+.0.`,
		live:         "means _no deadline was supplied_, which is the no-cap case above; it is never a deadline of `0`.",
		tokenRemoved: "means _no deadline was supplied_, which is the no-cap case above.",
	},
	{
		name:         "override-spends-once-through-run-state",
		pattern:      `The\s+override\s+spends\s+once\.\*\*\s+.overrunRoundsUsed.\s+is\s+run\s+state`,
		live:         "**The override spends once.** `overrunRoundsUsed` is run state the loop increments",
		tokenRemoved: "**The override spends once.** Each new finding starts it over.",
	},
	{
		name:         "override-not-reset-by-finding-phase-or-round",
		pattern:      `not\s+reset\s+by\s+a\s+new\s+finding,\s+a\s+new\s+phase,\s+or\s+a\s+new\s+round`,
		live:         "not reset by a new finding, a new phase, or a new round.",
		tokenRemoved: "not reset by a new round.",
	},
	{
		// "exactly one of these three" is what closes the list. "one of these causes" invites a
		// fourth, and the fourth every timed run reaches for is the clock.
		name:         "terminal-open-mustfix-has-exactly-three-causes",
		pattern:      `and\s+exactly\s+one\s+of\s+these\s+three\s+causes`,
		live:         "its `<file:line> - <title>` — and exactly one of these three causes:",
		tokenRemoved: "its `<file:line> - <title>` — and one of these causes:",
	},
	{
		// AC5's three causes are pinned one apiece, not as a list: an enumeration pin alone goes
		// green when a cause is dropped and the count word edited with it, and each cause licenses
		// a different terminal state. Cause 1 is the one that must name DISPATCH-and-survival --
		// "the fix subagent reported failure" would also cover a round that was never run.
		name:         "cause-1-attempted-and-not-cleared",
		pattern:      `\*\*Attempted\s+and\s+not\s+cleared\*\*\s+—\s+a\s+fix\s+round\s+was\s+dispatched\s+against\s+it\s+and\s+it\s+survived`,
		live:         "**Attempted and not cleared** — a fix round was dispatched against it and it survived, including",
		tokenRemoved: "**Attempted and not cleared** — the fix subagent reported failure, including",
	},
	{
		// Cause 2 has to point at the effective cap the toolbox computes. "the round budget feels
		// spent" is the same sentence with the check removed.
		name:         "cause-2-round-cap-reached",
		pattern:      `\*\*Round\s+cap\s+reached\*\*\s+—\s+the\s+effective\s+cap\s+from\s+.bs-review-caps\.mjs\s+rounds.\s+is\s+spent`,
		live:         "**Round cap reached** — the effective cap from `bs-review-caps.mjs rounds` is spent, so no further",
		tokenRemoved: "**Round cap reached** — the round budget feels spent, so no further",
	},
	{
		// Cause 3 is the hard-ABORT route staying legal, and it turns on what the run MAY do, not
		// on how the finding feels: "looks risky" would readmit the clock's excuse by another name.
		name:         "cause-3-ineligible-to-attempt-at-all",
		pattern:      `\*\*Ineligible\s+to\s+attempt\s+at\s+all\*\*\s+—\s+the\s+fix\s+is\s+outside\s+what\s+this\s+run\s+may\s+do`,
		live:         "**Ineligible to attempt at all** — the fix is outside what this run may do, so no round could",
		tokenRemoved: "**Ineligible to attempt at all** — the fix looks risky, so no round could",
	},
	{
		name:         "clock-is-not-a-lawful-cause",
		pattern:      `.The\s+clock\s+ran\s+out.\s+is\s+\*\*not\*\*\s+on\s+that\s+list\s+and\s+never\s+becomes\s+a\s+fourth\s+entry`,
		live:         `"The clock ran out" is **not** on that list and never becomes a fourth entry.`,
		tokenRemoved: `"The clock ran out" is **not** on that list in most cases.`,
	},
	{
		// The earlier gate-failure paragraph can still use caller deadline for the skipped leg, but
		// must not assign that disposition to open must-fixes. This is the contradiction that escaped
		// the three per-cause pins below.
		name:         "caller-deadline-is-skipped-leg-not-open-mustfix-disposition",
		pattern:      `The\s+caller\s+deadline\s+is\s+the\s+disposition\s+for\s+the\s+skipped\s+leg,\s+not\s+for\s+an\s+open\s+must-fix`,
		live:         "The caller deadline is the disposition for the skipped leg, not for an\nopen must-fix",
		tokenRemoved: "with every still-open must-fix recorded as\n`unresolved (caller deadline)`",
	},
	{
		name:         "spent-override-reported-as-cause-one-or-two",
		pattern:      `not\s+as\s+a\s+bare\s+.unresolved\s+\(caller\s+deadline\).`,
		live:         "say\nso as cause 1 or 2 with the overrun ledger showing it, not as a bare `unresolved (caller deadline)`.",
		tokenRemoved: "say so as cause 1 or 2 with the overrun ledger showing it.",
	},
})

// bossReviewPhase6OverrunPins pins the Phase 6 half: the round-entry gate is where the refusal used
// to happen, so the exception has to be legible at the point of decision and not only in the
// deadline chapter a run may never re-read. `attempted` is the load-bearing definition -- keyed on
// DISPATCH, not on outcome -- because every wrong reading of it collapses the override to nothing
// (a fresh finding counted as attempted) or to everything (a stale one counted as unattempted).
var bossReviewPhase6OverrunPins = regProsePins([]falsificationProsePin{
	{
		name:         "phase6-override-is-the-same-call-without-a-deadline",
		pattern:      `bs-review-caps\.mjs\s+admit-fix-round.,\s+and\s+it\s+is\s+the\s+same\s+call\s+whether\s+or\s+not\s+a\s+deadline\s+was\s+supplied`,
		live:         "`bs-review-caps.mjs admit-fix-round`, and it is the same call whether or not a deadline was\n     supplied",
		tokenRemoved: "`bs-review-caps.mjs admit-fix-round`, which applies only when a deadline was supplied",
	},
	{
		name:         "phase6-attempted-means-dispatched-against",
		pattern:      `\*\*Attempted\*\*\s+means\s+a\s+fix\s+round\s+has\s+been\s+dispatched\s+against\s+that\s+specific`,
		live:         "**Attempted** means a fix round\n     has been dispatched against that specific `<file:line> - <title>` — whether or not it succeeded",
		tokenRemoved: "**Attempted** means a fix round has cleared that specific `<file:line> - <title>`",
		alsoRemoved: []string{
			"**Attempted** means a must-fix has been located against that specific `<file:line> - <title>`",
		},
	},
	{
		name:         "phase6-fresh-finding-is-unattempted",
		pattern:      `just\s+produced\s+has\s+\*\*not\*\*\s+been\s+attempted`,
		live:         "A finding the round now ending\n     just produced has **not** been attempted, so it is exactly what the override exists for",
		tokenRemoved: "A finding the round now ending just produced has been attempted",
	},
	{
		name:         "phase6-override-once-and-never-over-the-round-cap",
		pattern:      `available\s+\*\*once\*\*\s+per\s+run\s+and\s+never\s+overrides\s+the\s+round\s+cap`,
		live:         "is available **once** per run and never overrides the round cap.",
		tokenRemoved: "is available **once** per run.",
	},
	{
		name:         "phase6-caller-deadline-disposition-requires-a-spent-override",
		pattern:      `which,\s+after\s+the\s+override,\s+means\s+the\s+run\s+had\s+already\s+spent\s+the\s+override\s+or\s+was\s+already\s+at\s+the\s+round\s+cap`,
		live:         "**only** where no round could be spent on it at all — which, after the override, means the run\n     had already spent the override or was already at the round cap.",
		tokenRemoved: "**only** where no round could be spent on it at all.",
	},
	{
		// The zero-rounds outcome stays sanctioned, but only for a pass with nothing to override
		// for. Unscoped, it is the exact licence the old gate used to skip the fix loop entirely.
		name:         "phase6-zero-rounds-scoped-to-no-open-mustfix",
		pattern:      `\*\*and\s+the\s+pass\s+found\s+no\s+must-fix\s+to\s+override\s+for\*\*`,
		live:         "smaller than one initial pass plus one `FIX_ROUND_MINUTES` **and the pass found no must-fix to\n   override for**.",
		tokenRemoved: "smaller than one initial pass plus one `FIX_ROUND_MINUTES`.",
	},
	{
		name:         "phase6-zero-rounds-with-an-unattempted-mustfix-is-unlawful",
		pattern:      `zero\s+rounds\s+with\s+an\s+open,\s+never-attempted\s+must-fix.\s+is\s+\*\*not\*\*\s+a\s+lawful\s+outcome`,
		live:         "so \"zero rounds with an open, never-attempted\n   must-fix\" is **not** a lawful outcome",
		tokenRemoved: "so \"zero rounds with an open, never-attempted must-fix\" is a lawful outcome",
	},
})

// TestBossReviewMustFixOverrunAdmission asserts the bounded override against the real payload, in
// both windows it has to hold in. The windows are deliberate: sectionBetween refuses a span that
// would cross an intervening section, so a rule relocated out of §Caller deadline or out of Phase 6
// fails here rather than passing as a move -- and a run reads exactly one of those two places at
// the moment it decides whether to enter a round.
func TestBossReviewMustFixOverrunAdmission(t *testing.T) {
	const skillPath = "skills/boss-review/SKILL.md"

	for payloadName, payload := range shippedPayloads(t) {
		payloadName, payload := payloadName, payload
		t.Run(payloadName, func(t *testing.T) {
			skill := readPayloadFile(t, payload, skillPath)

			deadline := sectionBetween(t, skill, "## Caller deadline (wall-clock cap)", "## Findings contract")
			assertFalsificationPins(t, deadline, bossReviewMustFixOverrunPins)

			phase6 := sectionBetween(t, skill, "## Phase 6 — Fix must-fix", "## Phase 7")
			assertFalsificationPins(t, phase6, bossReviewPhase6OverrunPins)

			// The ledger field is how a caller tells a lawful overrun from an ignored deadline.
			// Without it the override is unobservable downstream and cause 1/2 above is unprovable.
			assertContains(t, skill, `"overrun": { "rounds": 0 | 1, "seconds": 0 | 1200, "reason": "mustfix-override" }`)

			// The Phase 6 leg allowance must stay subject to the override in the legs list; a legs
			// entry that still reads as an unconditional price is the pre-change gate restated.
			assertContains(t, skill, "subject to the single bounded")
		})
	}
}
