package setupscript

import (
	"fmt"
	"path/filepath"

	"github.com/recurser/bossalib/contract/plugin/loginshell"
)

// BuildArgv resolves the command Execute would run, including login-shell
// wrapping. Legacy shell strings materialize under .boss using legacyName.
func (s Spec) BuildArgv(opts ExecuteOpts, legacyName string) ([]string, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if opts.WorktreePath == "" {
		return nil, fmt.Errorf("%w: worktree path required", ErrInvalidSpec)
	}
	if legacyName == "" || filepath.Base(legacyName) != legacyName || legacyName == "." || legacyName == ".." {
		return nil, fmt.Errorf("%w: legacy filename must be a basename", ErrInvalidSpec)
	}
	argv, err := s.buildArgv(opts, legacyName)
	if err != nil {
		return nil, err
	}
	if opts.LoginShell != "" && loginshell.IsSupported(opts.LoginShell) {
		argv = loginshell.Wrap(opts.LoginShell, loginshell.Flags(opts.LoginShell), argv)
	}
	return argv, nil
}
