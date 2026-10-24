package skills

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/recurser/boss/internal/accountflow"
	"github.com/recurser/bossalib/buildinfo"
	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type Group struct {
	newClient                                           ClientFactory
	Config                                              Config
	skillPayloadBuildInfo                               func() (commit, version string)
	StartupIsTerminal                                   func() bool
	NewInitCronEnv                                      func() CronEnv
	StartupLookPath                                     func(string) (string, error)
	checkoutSkillSourceRewriteLockClaimRecoveryAcquired func()
	initNewLinearAdmin                                  func(key string) linearAdmin
	StartupReadAnswer                                   func() string
	checkoutSkillSourceRewriteLockClaimPublished        func(string)
	checkoutSkillSourceRewriteLockBeforeRecoveryReclaim func()
	skillInstallAgents                                  []skillInstallAgent
	skillPayloadGit                                     func(repoRoot string, args ...string) (string, error)
	InitIsTerminal                                      func() bool
	initNewPrompter                                     func(c *cobra.Command) accountflow.Prompter
	initWatchInterrupt                                  func(iv *initInterview) func()
	checkoutSkillSourceRewriteLockBeforeReclaim         func()
	skillDriftHistoryGit                                func(repoRoot string, args ...string) (string, error)
}

func New(factory ClientFactory, cfg Config) *Group {
	g := &Group{newClient: factory, Config: cfg}
	g.NewInitCronEnv = DefaultCronEnv
	g.StartupLookPath = exec.LookPath
	g.checkoutSkillSourceRewriteLockClaimRecoveryAcquired = func() {}
	g.initNewLinearAdmin = newLinearAdmin
	g.StartupReadAnswer = func() string {
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil {
			return ""
		}
		return answer
	}
	g.checkoutSkillSourceRewriteLockClaimPublished = func(string) {}
	g.checkoutSkillSourceRewriteLockBeforeRecoveryReclaim = func() {}
	g.skillInstallAgents = []skillInstallAgent{
		{name: "Claude", command: "claude", agent: libskillinstall.AgentClaude},
		{name: "Codex", command: "codex", agent: libskillinstall.AgentCodex},
		{name: "Hermes", command: "hermes", agent: libskillinstall.AgentHermes},
	}
	g.skillPayloadGit = checkoutGitOutput
	g.InitIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	g.initNewPrompter = func(c *cobra.Command) accountflow.Prompter {
		return accountflow.NewIOPrompter(c.InOrStdin(), c.OutOrStdout())
	}
	g.initWatchInterrupt = watchInitInterrupt
	g.checkoutSkillSourceRewriteLockBeforeReclaim = func() {}
	g.skillDriftHistoryGit = checkoutGitOutput
	g.skillPayloadBuildInfo = func() (commit, version string) {
		return buildinfo.Commit, buildinfo.Version
	}
	g.StartupIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	return g
}
