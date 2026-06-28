package server

import (
	"fmt"
	"strings"
)

const maxModelLen = 200

// validateModel intentionally does NOT enumerate model names: new and
// deprecated models must work with zero code change. The agent CLI is the
// source of truth and rejects unknown models at runtime. The only constraint
// here is a sanity length cap to bound stored/forwarded data.
func (s *Server) validateModel(model string) error {
	if len(strings.TrimSpace(model)) > maxModelLen {
		return fmt.Errorf("model too long (max %d chars)", maxModelLen)
	}
	return nil
}
