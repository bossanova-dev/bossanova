# Falsification probes

Use this recipe to prove that a gate, guard, or assertion is load-bearing. The shared checklist is
mandatory and ordered. Prefer the zero-write tier. A read-only reviewer may use Tier A only. Tier B
is for orchestrator, fix, and repair paths only.

## Shared checklist

1. **Name the property.** State what the gate claims to forbid. A one-sided bound must name the
   direction it does not bound; for example, a minimum does not constrain larger values.
2. **Mutate the production feed, never the assertion.** Break the input the gate measures, whether
   that input is a source conditional, fixture, literal shell line, or prose sentence. Changing the
   gate's own expected value does not prove the production feed is connected.
3. **Prove the mutation landed.** Do this before reading the gate result. For Tier B, git diff
   --numstat -- "<absolute path>" must be non-empty. For Tier A, cmp -s on the original and copy
   must fail with exit 1: the inputs differ. Treat exit 2 as a harness error, not evidence of a mutation. An
   exit-zero replacement that matched nothing is not a probe.
4. **Require red for the right reason.** The failure must name the property from step 1. A compile
   error, module-resolution error, or harness error is not a kill; those show that the build broke,
   not that the gate detected the mutation.
5. **Restore exactly, then prove the restore.** Verify the original path or scratch copy is exact,
   verify the checkout path is clean when Tier B was used, and re-run the gate green before
   recording a conclusion.

## Tier A — zero-write probe

Tier A writes only under a private per-probe scratch root; it never touches the checkout. Create the
root before making any Tier-A artifact. A relative `$TMPDIR` is not a trusted root, so use `/tmp` in
that case. Canonicalize both roots, reject a temporary root inside the checkout, and check the
created directory again before using it:

```sh
CHECKOUT_ROOT_INPUT=${BOSS_REVIEW_CHECKOUT_ROOT:-}
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
CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}
if [ "$CHECKOUT_ROOT" = / ]; then
  printf '%s\n' "Tier A cannot safely isolate a checkout rooted at /" >&2
  exit 1
fi
TMP_ROOT=${BOSS_REVIEW_TIER_A_ROOT:-${TMPDIR:-/tmp}}
case "$TMP_ROOT" in
  /*) ;;
  *) TMP_ROOT=/tmp ;;
esac
TMP_ROOT_OUTPUT=$(cd "$TMP_ROOT" && printf '%sx' "$PWD") || exit 1
TMP_ROOT=${TMP_ROOT_OUTPUT%x}
case "$TMP_ROOT" in
  "$CHECKOUT_ROOT"|"$CHECKOUT_ROOT"/*)
    printf '%s\n' "Tier A temporary root is inside the checkout" >&2
    exit 1
    ;;
esac
PROBE_DIR=
cleanup_probe_dir() {
  probe_status=$1
  trap - EXIT HUP INT TERM
  if [ -n "${PROBE_DIR:-}" ] && [ -e "$PROBE_DIR" ]; then
    rm -rf -- "$PROBE_DIR" || exit 1
  fi
  exit "$probe_status"
}
# Register cleanup before adding any probe artifacts. POSIX shells do not run an
# EXIT-only trap for SIGTERM.
trap 'cleanup_probe_dir $?' EXIT
trap 'cleanup_probe_dir 129' HUP
trap 'cleanup_probe_dir 130' INT
trap 'cleanup_probe_dir 143' TERM
PROBE_DIR=$(mktemp -d "$TMP_ROOT/boss-review-tier-a.XXXXXX") || exit 1
PROBE_DIR=$(cd "$PROBE_DIR" && pwd -P) || exit 1
case "$PROBE_DIR" in
  "$CHECKOUT_ROOT"|"$CHECKOUT_ROOT"/*)
    printf '%s\n' "Tier A scratch root is inside the checkout" >&2
    exit 1
    ;;
esac
```

All Tier-A manifests, replacement sources, copied package trees, scripts, and cleanup stay beneath
`"$PROBE_DIR"`. Never use shared `$TMPDIR/<name>` paths: concurrent probes
can overwrite each other.

For shell and interpreted-source probes, changing into `"$PROBE_DIR"` is not filesystem isolation. An
absolute path or an inherited variable such as `$OUTPUT` can still target the checkout or another host
path. Run a Tier-A shell or interpreted-source probe only in a filesystem sandbox whose writable paths
are limited to `"$PROBE_DIR"` and whose readable view is an explicit allowlist: the probe root, copied
fixture, required tool binaries and libraries, and any read-only dependency cache. Do not expose host
credentials, home directories, or arbitrary host paths. Mount any copied fixture read-only, start with
a cleared environment (including `HOME`, `TMPDIR`, `PWD`, and XDG paths under `"$PROBE_DIR"`), and deny
writes outside the sandbox root. If that read and write confinement is unavailable, or the line needs a
path that cannot be copied or mounted beneath `"$PROBE_DIR"`, reject the probe; it is not a zero-write
probe.

Network access is disabled for every Tier-A probe, including loopback, link-local, and metadata
endpoints. The filesystem allowlist does not confine sockets or prevent external side effects. If network
isolation is unavailable, reject the probe rather than executing copied code on the host.

- **Go:** use `go test -overlay` with a `$PROBE_DIR` replacement file only for a command known not
  to write to its working directory. Redirect Go's cache, temporary, home, and XDG state beneath
  `$PROBE_DIR`, and use `-mod=readonly` so the command cannot update `go.mod` or `go.sum` in the
  checkout. An empty `GOMODCACHE` cannot resolve dependencies in readonly mode: reuse the existing
  module cache through a read-only sandbox mount. Build `$PROBE_DIR/overlay.json` as a JSON overlay
  manifest:

  ```json
  { "Replace": { "<absolute original path>": "<absolute replacement source path>" } }
  ```

  Then, after creating the private state directories, run:

  ```sh
  GO_MOD_CACHE=$(go env GOMODCACHE) || exit 1
  test -d "$GO_MOD_CACHE" || { echo "Go module cache is unavailable" >&2; exit 1; }
  # GO_MOD_CACHE must be mounted read-only in the filesystem sandbox at this same path.
  mkdir -p "$PROBE_DIR/go-cache" "$PROBE_DIR/go-tmp" \
    "$PROBE_DIR/go-path" "$PROBE_DIR/home" "$PROBE_DIR/xdg-cache" \
    "$PROBE_DIR/xdg-config" "$PROBE_DIR/xdg-data" || exit 1
  env -i PATH="$PATH" HOME="$PROBE_DIR/home" TMPDIR="$PROBE_DIR/go-tmp" \
    XDG_CACHE_HOME="$PROBE_DIR/xdg-cache" XDG_CONFIG_HOME="$PROBE_DIR/xdg-config" \
    XDG_DATA_HOME="$PROBE_DIR/xdg-data" GOPATH="$PROBE_DIR/go-path" \
    GOCACHE="$PROBE_DIR/go-cache" GOTMPDIR="$PROBE_DIR/go-tmp" \
    GOMODCACHE="$GO_MOD_CACHE" GOENV=off GOTELEMETRY=off \
    go test -mod=readonly -overlay "$PROBE_DIR/overlay.json" ...
  ```

  The `-overlay` argument is the manifest, never the replacement source file. Run this command in a
  filesystem sandbox with an explicit readable allowlist and only `$PROBE_DIR` writable; mount the
  checkout and module cache read-only, and do not expose host credential or home paths. The Go variables
  do not confine test subprocesses. If that sandbox is unavailable, or the test or command can write
  relative paths, copy its runnable package tree to `$PROBE_DIR` and run there instead; if the package
  cannot be isolated, reject the Tier-A probe.

- **Interpreted source with sibling imports:** `cp -R` the whole directory to `$PROBE_DIR` so sibling
  imports resolve, mutate the copy, and run from the copy. Never run `find` or `cp -R` against the
  checkout source directly: either can update atime. Before the probe, have the filesystem sandbox
  supply `SOURCE_VIEW`, a read-only, `noatime` view of `$SOURCE_DIR` with the same source topology.
  Reject the probe when that atime-safe view is unavailable; a plain read-only copy or bind mount is
  insufficient unless it also prevents atime updates. Before copying, reject the probe if
  `SOURCE_VIEW` contains any symlink; `cp -R` retains symlinks, and a retained symlink can escape
  `$PROBE_DIR` so a mutation or writer affects the original target. Do not dereference symlinks as
  a fallback: an external target is not an isolated scratch fixture. For a symlink-free source view,
  use `cp -R`:

  ```sh
  SOURCE_VIEW="<read-only noatime view of SOURCE_DIR>"
  if [ ! -d "$SOURCE_VIEW" ] || [ -L "$SOURCE_VIEW" ]; then
    echo "Tier A needs a read-only noatime source view" >&2
    exit 1
  fi
  SYMLINK_LIST="$PROBE_DIR/symlinks"
  if ! find "$SOURCE_VIEW" -type l -print >"$SYMLINK_LIST"; then
    echo "Tier A could not inspect source tree for symlinks" >&2
    exit 1
  fi
  if [ -s "$SYMLINK_LIST" ]; then
    echo "Tier A cannot copy a source tree containing symlinks" >&2
    exit 1
  fi
  cp -R "$SOURCE_VIEW" "$PROBE_DIR/"
  ```

  Execute the copied interpreted source in the same cleared-environment filesystem sandbox: an explicit
  readable allowlist, writable paths limited to `"$PROBE_DIR"`, no host credentials or home paths, and
  deny writes outside the sandbox root. Running from the copied directory is not confinement: absolute
  paths and ambient variables can still escape it. If that confinement is unavailable, reject the probe
  rather than execute it on the host.

  Use git show HEAD:<path> to materialize the pre-fix version under `$PROBE_DIR` only when HEAD is
  the known pre-change revision (for example, while the fix is still uncommitted); otherwise use git
  show <pre-fix revision>:<path>. Reading it is not evidence, so run it against the scratch fixture.

- **Shell lines:** extract the literal shell line from the subject, put it in
  `$PROBE_DIR/script.sh`, and run it inside the filesystem sandbox described above. The sandbox command
  must start from the private scratch directory, never the checkout:

  ```sh
  # Configure the host's sandbox with an explicit readable allowlist and "$PROBE_DIR" as its only writable path,
  # then execute: (cd "$PROBE_DIR" && env -i HOME="$PROBE_DIR/home" \
  # TMPDIR="$PROBE_DIR/tmp" PWD="$PROBE_DIR" sh "$PROBE_DIR/script.sh")
  ```

  Never run the probe directly from the checkout or from an unsandboxed host shell.
  Never use inline sh -c: an inline harness re-quotes the subject and can fail for a reason outside
  the line being tested. Copy every relative-path dependency beneath `$PROBE_DIR`; if dependencies
  cannot be copied or otherwise isolated beneath `"$PROBE_DIR"`, reject the probe rather than run it
  from the checkout.

- **Path-taking detectors:** point the detector at the mutated copy under `$PROBE_DIR`.

After mutating, require cmp -s on the original and copy to exit 1 (inputs differ); exit 2 is a
harness error, not a mutation. Then require the expected gate failure, discard the scratch directory
by its exact path, and run the unchanged gate green.

## Tier B — in-place mutation

Tier B is restricted to state-owning orchestrator, fix, and repair paths.

1. **Commit the work first.** The probe and repository tooling may restore tracked paths. git
   checkout -- <file> discards uncommitted edits that share the file, often without output.
2. Resolve the checkout root and target to absolute paths. Set `PROBE_TARGET` to the exact target,
   create a private backup directory before mutation, and register cleanup that restores the target
   before deleting the backup:

   ```sh
   # Git emits a record newline before the sentinel. Remove that delimiter and
   # the sentinel together so a newline belonging to the checkout root remains.
   CHECKOUT_ROOT_OUTPUT=$(git rev-parse --show-toplevel && printf x) || exit 1
   CHECKOUT_ROOT_RECORD_SUFFIX=$(printf '\nx')
   CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%"$CHECKOUT_ROOT_RECORD_SUFFIX"}
   CHECKOUT_ROOT_OUTPUT=$(cd "$CHECKOUT_ROOT" && printf '%sx' "$PWD") || exit 1
   CHECKOUT_ROOT=${CHECKOUT_ROOT_OUTPUT%x}
   if [ "$CHECKOUT_ROOT" = / ]; then
     printf '%s\n' "Tier B cannot safely contain a checkout rooted at /" >&2
     exit 1
   fi
   TMP_ROOT=${TMPDIR:-/tmp}
   case "$TMP_ROOT" in
     /*) ;;
     *) TMP_ROOT=/tmp ;;
   esac
   # Append and remove a sentinel so command substitution preserves a temporary
   # root ending in a newline.
   TMP_ROOT_OUTPUT=$(cd -P "$TMP_ROOT" && printf '%sx' "$PWD") || exit 1
   TMP_ROOT=${TMP_ROOT_OUTPUT%x}
   case "$TMP_ROOT" in
     "$CHECKOUT_ROOT"|"$CHECKOUT_ROOT"/*)
       printf '%s\n' "Tier B temporary root is inside the checkout" >&2
       exit 1
       ;;
   esac
   PROBE_TARGET="<absolute path>"
   # Parameter expansion preserves newlines; command substitution around dirname does not.
   PROBE_PARENT_INPUT=${PROBE_TARGET%/*}
   if [ -z "$PROBE_PARENT_INPUT" ]; then PROBE_PARENT_INPUT=/; fi
   # Append and remove a sentinel so command substitution preserves a parent ending in a newline.
   PROBE_PARENT_LOGICAL=$(cd -L "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD") || exit 1
   PROBE_PARENT_LOGICAL=${PROBE_PARENT_LOGICAL%x}
   PROBE_PARENT=$(cd -P "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD") || exit 1
   PROBE_PARENT=${PROBE_PARENT%x}
   if [ "$PROBE_PARENT_LOGICAL" != "$PROBE_PARENT" ] || [ -L "$PROBE_TARGET" ]; then
     printf '%s\n' "Tier B probe target must not be or resolve through a symlink" >&2
     exit 1
   fi
   probe_parent_identity() {
     stat -c '%d:%i' "$PROBE_PARENT" 2>/dev/null || \
       stat -f '%d:%i' "$PROBE_PARENT" 2>/dev/null
   }
   PROBE_PARENT_ID=$(probe_parent_identity) || {
     printf '%s\n' "Tier B could not determine probe parent identity" >&2
     exit 1
   }
   validate_probe_parent() {
     PROBE_PARENT_LOGICAL_NOW=$(cd -L "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD") || return 1
     PROBE_PARENT_LOGICAL_NOW=${PROBE_PARENT_LOGICAL_NOW%x}
     PROBE_PARENT_NOW=$(cd -P "$PROBE_PARENT_INPUT" && printf '%sx' "$PWD") || return 1
     PROBE_PARENT_NOW=${PROBE_PARENT_NOW%x}
     [ "$PROBE_PARENT_LOGICAL_NOW" = "$PROBE_PARENT" ] || return 1
     [ "$PROBE_PARENT_NOW" = "$PROBE_PARENT" ] || return 1
     PROBE_PARENT_ID_NOW=$(probe_parent_identity) || return 1
     [ "$PROBE_PARENT_ID_NOW" = "$PROBE_PARENT_ID" ]
   }
   # Do not use command substitution for the basename: it strips trailing newlines.
   PROBE_BASENAME=${PROBE_TARGET##*/}
   PROBE_TARGET="$PROBE_PARENT/$PROBE_BASENAME"
   case "$PROBE_PARENT" in
     "$CHECKOUT_ROOT"|"$CHECKOUT_ROOT"/*) ;;
     *)
       printf '%s\n' "Tier B probe target is outside the checkout" >&2
       exit 1
       ;;
   esac
   if [ "$PROBE_TARGET" = "$CHECKOUT_ROOT" ]; then
     printf '%s\n' "Tier B probe target must not be the checkout root" >&2
     exit 1
   fi
   if [ ! -f "$PROBE_TARGET" ]; then
     printf '%s\n' "Tier B probe target must be a regular file" >&2
     exit 1
   fi
   git -C "$CHECKOUT_ROOT" ls-files --error-unmatch -- \
     ":(literal)$PROBE_TARGET" >/dev/null || {
       printf '%s\n' "Tier B probe target must be an exact tracked path" >&2
       exit 1
     }
   PROBE_LINK_COUNT=$(stat -c '%h' "$PROBE_TARGET" 2>/dev/null || \
     stat -f '%l' "$PROBE_TARGET" 2>/dev/null) || {
       printf '%s\n' "Tier B could not determine probe target link count" >&2
       exit 1
     }
   case "$PROBE_LINK_COUNT" in
     1) ;;
     ''|*[!0-9]*)
       printf '%s\n' "Tier B probe target link count is invalid" >&2
       exit 1
       ;;
     *)
       printf '%s\n' "Tier B probe target must not have hard links" >&2
       exit 1
       ;;
   esac
   PROBE_MOUNTPOINT=$(command -v mountpoint) || {
     printf '%s\n' "Tier B needs mountpoint to reject mounted probe targets" >&2
     exit 1
   }
   probe_target_is_not_mount() {
     "$PROBE_MOUNTPOINT" -q -- "$PROBE_TARGET"
     PROBE_MOUNT_STATUS=$?
     # util-linux mountpoint uses 32 for an existing non-mount. Status 1 is
     # an error, not a non-mount result, so only 32 may proceed.
     case "$PROBE_MOUNT_STATUS" in
       32) return 0 ;;
       *) return 1 ;;
     esac
   }
   validate_probe_target() {
     validate_probe_parent || return 1
     [ -f "$PROBE_TARGET" ] || return 1
     [ ! -L "$PROBE_TARGET" ] || return 1
     probe_target_is_not_mount
   }
   validate_probe_target_for_removal() {
     validate_probe_parent || return 1
     # rm unlinks a symlink instead of traversing it. An absent target is also
     # safe: cleanup is idempotent after a mutation that already removed it.
     [ -L "$PROBE_TARGET" ] && return 0
     [ ! -e "$PROBE_TARGET" ] && return 0
     # Never recursively remove a replacement: it could contain a mounted
     # descendant whose contents are outside the checkout.
     [ -f "$PROBE_TARGET" ] || return 1
     probe_target_is_not_mount
   }
   validate_probe_target_for_restore() {
     validate_probe_parent || return 1
     # The replacement must be absent so tar cannot follow it during restore.
     [ ! -e "$PROBE_TARGET" ] && [ ! -L "$PROBE_TARGET" ]
   }
   validate_probe_target || {
     printf '%s\n' "Tier B probe target must not be a mount point" >&2
     exit 1
   }
   PROBE_DIR=$(mktemp -d "$TMP_ROOT/boss-review-probe.XXXXXX") || exit 1
   PROBE_DIR=$(cd "$PROBE_DIR" && pwd -P) || exit 1
   case "$PROBE_DIR" in
     "$CHECKOUT_ROOT"|"$CHECKOUT_ROOT"/*)
       rmdir "$PROBE_DIR" || exit 1
       printf '%s\n' "Tier B backup directory is inside the checkout" >&2
       exit 1
       ;;
   esac
   # Select a host-native backup implementation before the target becomes mutable.
   # Do not substitute cp -pP: it cannot preserve every extended metadata class.
   case "$(uname -s)" in
     Linux)
       PROBE_TAR=$(command -v tar) || {
         printf '%s\n' "Tier B needs GNU tar for an exact metadata backup" >&2
         exit 1
       }
       "$PROBE_TAR" --version 2>/dev/null | grep -q 'GNU tar' || {
         printf '%s\n' "Tier B needs GNU tar for an exact metadata backup" >&2
         exit 1
       }
       PROBE_ATIME=$(stat -c '%x' "$PROBE_TARGET") || {
         printf '%s\n' "Tier B could not capture probe target access time" >&2
         exit 1
       }
       PROBE_MODE=$(stat -c '%a' "$PROBE_TARGET") || {
         printf '%s\n' "Tier B could not capture probe target mode" >&2
         exit 1
       }
       PROBE_TOUCH=$(command -v touch) || {
         printf '%s\n' "Tier B needs GNU touch for exact access-time restore" >&2
         exit 1
       }
       "$PROBE_TOUCH" --version 2>/dev/null | grep -q 'GNU coreutils' || {
         printf '%s\n' "Tier B needs GNU touch for exact access-time restore" >&2
         exit 1
       }
       restore_probe_atime() {
         "$PROBE_TOUCH" -a -d "$PROBE_ATIME" -- "$PROBE_TARGET"
       }
       verify_probe_mode() {
         PROBE_RESTORED_MODE=$(stat -c '%a' "$PROBE_TARGET") || return 1
         [ "$PROBE_RESTORED_MODE" = "$PROBE_MODE" ]
       }
       backup_probe_target() {
         validate_probe_target || return 1
         "$PROBE_TAR" --atime-preserve=system --xattrs --xattrs-include='*' --acls --selinux -cf \
           "$PROBE_DIR/probe.tar" -C "$PROBE_PARENT" "./$PROBE_BASENAME"
       }
       restore_probe_backup() {
         validate_probe_target_for_restore || return 1
         "$PROBE_TAR" --xattrs --xattrs-include='*' --acls --selinux --same-owner --same-permissions \
           --numeric-owner -xf \
           "$PROBE_DIR/probe.tar" -C "$PROBE_PARENT" &&
           restore_probe_atime &&
           verify_probe_mode
       }
       ;;
     Darwin)
       printf '%s\n' "Tier B exact access-time restore is unavailable on Darwin" >&2
       exit 1
       ;;
     *)
       printf '%s\n' "Tier B has no exact metadata backup implementation for this host" >&2
       exit 1
       ;;
   esac
   remove_mutated_probe_target() {
     validate_probe_target_for_removal || return 1
     rm -f -- "$PROBE_TARGET"
   }
   MUTATION_ACTIVE=0
   cleanup_probe() {
     probe_status=$1
     trap - EXIT HUP INT TERM
     if [ "${MUTATION_ACTIVE:-0}" = 1 ]; then
       if ! remove_mutated_probe_target; then
         printf '%s\n' "failed to remove mutated $PROBE_TARGET before restore" >&2
         exit 1
       fi
       if ! restore_probe_backup; then
         printf '%s\n' "failed to restore $PROBE_TARGET from probe backup" >&2
         exit 1
       fi
     fi
     if ! rm -rf -- "$PROBE_DIR"; then
       printf '%s\n' "failed to remove probe directory $PROBE_DIR" >&2
       exit 1
     fi
     exit "$probe_status"
   }
   # Install one-shot signal handlers before the target becomes mutable. POSIX
   # shells such as dash do not run an EXIT-only trap for SIGTERM.
   trap 'cleanup_probe $?' EXIT
   trap 'cleanup_probe 129' HUP
   trap 'cleanup_probe 130' INT
   trap 'cleanup_probe 143' TERM
   backup_probe_target || exit 1
   # Start the mutation immediately after this check; do not allow a later mount to replace the target.
   validate_probe_target || {
     printf '%s\n' "Tier B probe target became a mount point before mutation" >&2
     exit 1
   }
   MUTATION_ACTIVE=1
   ```

   Reject a target with any hard link or mount point before copying or mutating it. A bind-mounted
   regular file otherwise passes the regular-file, index, symlink, and link-count checks while writes
   escape the checkout and cleanup cannot remove the mount point. On util-linux, `mountpoint -q`
   returns `32` for an existing non-mount; a missing target, a mount point (`0`), status `1`, or
   any other status rejects a pre-mutation probe. Call `validate_probe_target` immediately before
   the mutation as well as before backup, so a target mounted after setup is rejected before it can
   be changed. Cleanup uses a separate validation path: it pins the parent and accepts only an
   absent target, a symlink, or an unmounted regular-file replacement. It rejects directories rather
   than recursively removing them, because a directory can contain a mounted descendant. `rm -f --`
   unlinks the permitted file or symlink without traversing it. Restore pins the parent and requires
   the target to be absent. The link count probe uses BSD
   `stat -f '%l'` and GNU `stat -c '%h'`; if neither can provide a numeric count, reject the probe
   rather than risking a write. A normal in-place mutation changes every hard link to the same inode,
   while the remove-and-copy restore only repairs `PROBE_TARGET` and breaks its original link
   relationship. `cp -pP` is not an exact backup of extended attributes, ACLs, or security contexts.
   Use a metadata-preserving backup and restore tool for the current host; it must preserve contents,
   ownership, mode, timestamps, extended attributes, ACLs, and security contexts. For example, a GNU
   host can archive with `tar --xattrs --xattrs-include='*' --acls --selinux -cf
"$PROBE_DIR/probe.tar" -C "$PROBE_PARENT" "./$PROBE_BASENAME"`, restore with the matching
   `tar --xattrs --xattrs-include='*' --acls --selinux --same-owner --same-permissions --numeric-owner -xf
"$PROBE_DIR/probe.tar" -C "$PROBE_PARENT"`, then restore atime with GNU `touch -a -d`.
   Check that the selected tool and effective credentials can preserve every attribute present on the
   target before mutation. GNU tar does not restore the archived target's atime: on Linux, capture
   `PROBE_ATIME` with `stat -c '%x'` before backup and make `restore_probe_backup` call GNU
   `touch -a -d "$PROBE_ATIME"` after extraction. Capture the target mode with `stat -c '%a'` before
   backup and verify the restored value after extraction before clearing `MUTATION_ACTIVE`. A later gate or verification can read the target
   and advance atime again, so after the final target-reading command, call `restore_probe_atime` and
   verify the saved atime with `stat -c '%x'`; fail closed if either command is unavailable or the
   values differ. Darwin has no verified exact access-time restore implementation here, so reject the
   probe rather than treating `ditto` as exact. If that metadata-preserving backup capability is
   unavailable, reject the probe; do not fall back to `cp -pP`.

   Name the selected commands `backup_probe_target` and `restore_probe_backup`; both must fail closed
   when their preservation guarantees cannot be met. After the mutation, remove the mutated destination
   with `remove_mutated_probe_target` before `restore_probe_backup`. Both revalidate the pinned parent
   identity immediately before every target removal and restore, and fail without traversing a changed
   parent. Removing first prevents the restore tool
   from following a symlink that the mutation left at the target path. `rm -rf --` removes a file,
   symlink, or directory replacement without following a symlink.
   After the mutation and gate run, explicitly restore with `remove_mutated_probe_target || exit 1`
   followed by `restore_probe_backup || exit 1`, then set
   `MUTATION_ACTIVE=0` only after exactness verification, checkout cleanliness, the green rerun,
   then a second exactness and checkout-cleanliness verification immediately after that final green
   gate, and the final `restore_probe_atime` all succeed. The second verification must repeat the
   content, mode, and metadata checks rather than reusing their pre-gate result; a green gate may
   regenerate the tracked target. Leave `MUTATION_ACTIVE` active when either post-green check
   differs so the trap can restore again from the retained backup. The trap removes the mutated exact target
   and restores it when a mutation is
   active (including on `set -e`, cancellation, or timeout). Register one-shot `HUP`, `INT`, and
   `TERM` handlers before the mutation starts, with their conventional nonzero exit statuses: POSIX
   shells such as `dash` do not run an `EXIT`-only trap when terminated. A failed restore exits nonzero
   rather than silently deleting the only backup.
   The backup remains authoritative even if a commit or command fails, and the trap deletes only that
   private directory.

3. Use absolute paths and never inherit the cwd from an earlier shell call. Otherwise a later
   pathspec or build can silently run relative to the wrong directory. Re-anchor each call when an
   absolute command path is unavailable.
4. Follow the shared checklist: mutate, prove the non-empty diff, require the named failure, restore
   from the backup, prove the path clean, and re-run green.
5. Delete scratch files by exact path, never by glob. An unmatched glob can abort cleanup and strand
   a deliberately broken file.

## Design-time input emptiness

For any gate whose scope is derived from the tree, ask: what would make this check's input set go
empty, and would that be visible? Derivation covers the widening direction only; a moved or renamed
root can shrink the set to nothing while the gate stays green. Keep a pinned list as a narrowing
tripwire and compare the derived and pinned sets in both directions.
