package skills

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/kit/appstate"
)

func extractSkillPayload(out io.Writer, agent, dir string, payload selectedSkillPayload, record libskillinstall.PayloadRecord, mode skillSyncMode) error {
	if mode != skillSyncSetAside {
		return libskillinstall.ExtractRecorded(dir, payload.fsys, record)
	}
	state, err := appstate.Dir()
	if err != nil {
		return err
	}
	// Subsecond precision avoids colliding with a second explicit repair during
	// the same second. The library refuses an existing destination.
	backup := filepath.Join(state, "skill-backups", agent, time.Now().UTC().Format("20060102T150405.000000000Z"))
	paths, err := libskillinstall.ExtractRecordedPreserving(dir, payload.fsys, record, backup)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		_, _ = fmt.Fprintf(out, "boss skills: %s nothing to set aside\n", agent)
	} else {
		_, _ = fmt.Fprintf(out, "boss skills: %s set aside %d path(s) to %s\n", agent, len(paths), backup)
	}
	return nil
}
