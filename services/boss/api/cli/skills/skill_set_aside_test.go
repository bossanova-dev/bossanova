package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

func TestSkillSetAside_BossSkills_AC8_AC9(t *testing.T) {
	g := newTestGroup()
	home := g.setupSkillStartupTest(t)
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, gateSkillFS())
	commitCheckoutAsOriginHead(t, root)
	t.Chdir(root)
	g.setAvailableSkillAgents(map[string]bool{"claude": true})
	dir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(dir, os.DirFS(srcRoot)); err != nil {
		t.Fatal(err)
	}
	ns := filepath.Join(dir, libskillinstall.Namespace)
	for rel, data := range map[string]string{"boss/SKILL.md": "never committed", "boss/extra": "unexpected", "boss-retired/notes": "stale"} {
		p := filepath.Join(ns, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	var gate bytes.Buffer
	if err := g.runSkillGate(&gate, "claude"); err == nil {
		t.Fatal("gate passed unexplained drift")
	}
	command := skillInstallRemedy(selectedSkillPayload{fromSource: true, srcRoot: srcRoot}) + " --set-aside --agent claude"
	if !strings.Contains(gate.String(), "`"+command+"`") || strings.Contains(gate.String(), "run `") {
		t.Fatalf("gate=%s", gate.String())
	}
	assertGateOutputHidesGateFlag(t, gate.String())
	var check bytes.Buffer
	_ = g.runSkillCheck(&check, "claude")
	if !strings.Contains(check.String(), "`"+command+"`") {
		t.Fatalf("check=%s", check.String())
	}
	var out bytes.Buffer
	cmd := g.skillsCmd()
	cmd.SetArgs([]string{"install", "--set-aside", "--agent", "claude"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set-aside=%v output=%s", err, out.String())
	}
	marker := "boss skills: claude set aside 3 path(s) to "
	var backup string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, marker) {
			backup = strings.TrimPrefix(line, marker)
		}
	}
	if backup == "" {
		t.Fatalf("output=%s", out.String())
	}
	if !strings.Contains(backup, filepath.Join("skill-backups", "claude")) {
		t.Fatalf("backup=%s", backup)
	}
	for rel, want := range map[string]string{"boss/SKILL.md": "never committed", "boss/extra": "unexpected", "boss-retired/notes": "stale"} {
		got, err := os.ReadFile(filepath.Join(backup, libskillinstall.Namespace, rel))
		if err != nil || string(got) != want {
			t.Fatalf("backup %s=%q %v", rel, got, err)
		}
	}
	gate.Reset()
	if err := g.runSkillGate(&gate, "claude"); err != nil {
		t.Fatalf("gate after reinstall=%v %s", err, gate.String())
	}
	out.Reset()
	cmd = g.skillsCmd()
	cmd.SetArgs([]string{"install", "--set-aside", "--agent", "claude"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to set aside") {
		t.Fatalf("output=%s", out.String())
	}
	entries, err := os.ReadDir(filepath.Dir(backup))
	if err != nil || len(entries) != 1 {
		t.Fatalf("backups=%v %v", entries, err)
	}
}
