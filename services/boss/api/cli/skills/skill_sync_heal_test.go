package skills

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

func TestRunSkillGateHeal(t *testing.T) {
	g := newTestGroup()

	for _, scenario := range []string{"recoverable", "mixed", "skip", "untrusted", "fallback", "self-edited", "self-edited-committed", "read-only", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			home := g.setupSkillStartupTest(t)
			root := t.TempDir()
			old := gateSkillFS()
			srcRoot := writeSkillSources(t, root, old)
			commitCheckoutAsOriginHead(t, root)
			dir := filepath.Join(home, ".claude", "skills")
			original := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginEmbedded, Revision: "abc1234", Writer: libskillinstall.WriterExplicit}
			if err := libskillinstall.ExtractRecorded(dir, old, original); err != nil {
				t.Fatal(err)
			}
			sourceFile := filepath.Join(srcRoot, "skills", "boss", "SKILL.md")
			current := []byte("new boss skill\n\n")
			if err := os.WriteFile(sourceFile, current, 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, root, "add", ".")
			runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "new payload")
			runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
			installedFile := filepath.Join(dir, libskillinstall.Namespace, "boss", "SKILL.md")
			absent := filepath.Join(dir, libskillinstall.Namespace, "boss-build", "SKILL.md")
			if err := os.Remove(absent); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unknown":
				saved := g.skillDriftHistoryGit
				g.skillDriftHistoryGit = func(string, ...string) (string, error) { return "", fmt.Errorf("history unavailable") }
				t.Cleanup(func() { g.skillDriftHistoryGit = saved })
			case "mixed":
				if err := os.WriteFile(absent, []byte("operator edit\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "skip":
				t.Setenv("BOSS_SKIP_SKILLS", "1")
			case "untrusted":
				runGit(t, root, "remote", "set-url", "origin", "https://github.com/example/other")
			case "fallback":
				runGit(t, root, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
			case "self-edited", "self-edited-committed":
				if err := os.WriteFile(sourceFile, []byte("branch-only\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if scenario == "self-edited-committed" {
					runGit(t, root, "add", ".")
					runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "branch changes")
				}
			}
			t.Chdir(root)
			var out bytes.Buffer
			err := g.runSkillGateWithHeal(&out, "claude", scenario != "read-only")
			assertGateOutputHidesGateFlag(t, out.String())
			got, readErr := os.ReadFile(installedFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			healed := scenario == "recoverable" || scenario == "mixed"
			if healed {
				if !bytes.Equal(got, current) || !strings.Contains(out.String(), "healed ") {
					t.Fatalf("not healed: %q; %s", got, &out)
				}
			} else if !bytes.Equal(got, old["skills/boss/SKILL.md"].Data) {
				t.Fatalf("unexpected write: %q", got)
			}
			record, present, recordErr := libskillinstall.ReadPayloadRecord(dir)
			if recordErr != nil || !present {
				t.Fatalf("record: %+v %v", record, recordErr)
			}
			if scenario == "recoverable" {
				if err != nil || strings.Contains(out.String(), "  - ") || record.Origin != libskillinstall.OriginCheckout || record.Writer != libskillinstall.WriterUnattended {
					t.Fatalf("heal result: %v %s %+v", err, &out, record)
				}
				var check bytes.Buffer
				if err := g.runSkillGate(&check, "claude"); err != nil {
					t.Fatal(err)
				}
			} else {
				if scenario != "self-edited" && scenario != "self-edited-committed" && err == nil {
					t.Fatalf("expected residual drift: %s", &out)
				}
				if record != original {
					t.Fatalf("partial record changed: %+v", record)
				}
			}
			if scenario == "mixed" && (!strings.Contains(out.String(), "reinstall withheld [lost]") || strings.Contains(out.String(), "boss/SKILL.md (content")) {
				t.Fatalf("residual: %s", &out)
			}
			if scenario == "skip" || scenario == "untrusted" || scenario == "fallback" {
				if !strings.Contains(out.String(), "heal skipped —") {
					t.Fatalf("no skip reason: %s", &out)
				}
			}
		})
	}
}

func TestRunSkillCheckHealRequiresGate(t *testing.T) {
	g := newTestGroup()

	cmd := g.skillsCmd()
	cmd.SetArgs([]string{"check", "--heal"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("heal without gate accepted")
	}
}

func TestRunSkillGateHealAhead(t *testing.T) {
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	root := t.TempDir()
	old := gateSkillFS()
	src := writeSkillSources(t, root, old)
	commitCheckoutAsOriginHead(t, root)
	newer := gateSkillFS()
	newer["skills/boss/SKILL.md"].Data = []byte("newer installed\n")
	if err := os.WriteFile(filepath.Join(src, "skills", "boss", "SKILL.md"), newer["skills/boss/SKILL.md"].Data, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "newer")
	runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	dir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(dir, newer); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "checkout", "--detach", "HEAD~1")
	t.Chdir(root)
	var out bytes.Buffer
	if err := g.runSkillGateWithHeal(&out, "claude", true); err == nil {
		t.Fatal("ahead accepted")
	}
	assertGateOutputHidesGateFlag(t, out.String())
	got, err := os.ReadFile(filepath.Join(dir, libskillinstall.Namespace, "boss", "SKILL.md"))
	// guidance-audit: allow — installed bytes and the CLI refusal are runtime behavior.
	if err != nil || !bytes.Equal(got, newer["skills/boss/SKILL.md"].Data) || !strings.Contains(out.String(), "reinstall withheld [downgrade]") {
		t.Fatalf("ahead changed: %q %v %s", got, err, &out)
	}
}
