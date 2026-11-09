package verifyjob

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

// skillsConfigFile is the repository-root skills config that holds verify's admission policy.
const skillsConfigFile = ".boss-skills.json"

// AdmitsBots reports whether the skills config in checkout dir admits bot-authored PRs to verify
// (verifyDefaults.admission.bot.admit is true), the same answer verify's candidates scan reads
// from its checkout through verifyAdmissionPolicy. A missing or unreadable file, an absent block
// and a malformed one all admit no bots, as they do for verify.
func AdmitsBots(dir string) bool {
	if dir == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, skillsConfigFile))
	if err != nil {
		return false
	}
	var doc struct {
		VerifyDefaults struct {
			Admission struct {
				Bot struct {
					Admit bool `json:"admit"`
				} `json:"bot"`
			} `json:"admission"`
		} `json:"verifyDefaults"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	return doc.VerifyDefaults.Admission.Bot.Admit
}

// OwnsBotMerges reports whether verify, rather than the daemon, merges repoID's bot PRs: it has an
// enabled verify job and the skills config in localPath admits bot PRs. Without bot admission
// verify never looks at a Dependabot PR, so the daemon must keep merging it.
func (c *Checker) OwnsBotMerges(ctx context.Context, repoID, localPath string) bool {
	return c.IsFactoryRepo(ctx, repoID) && AdmitsBots(localPath)
}
