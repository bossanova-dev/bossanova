package factoryhost

import (
	"os"
	"os/exec"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
)

type Host struct{}

func New() *Host { return &Host{} }

func (*Host) LookPath(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

func (*Host) SkillRoot(agent string) (string, error) {
	return skillinstall.DirForAgent(skillinstall.Agent(agent))
}

func (*Host) HomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func (*Host) FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
