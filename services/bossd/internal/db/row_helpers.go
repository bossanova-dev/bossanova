package db

import (
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
)

// textTime parses the SQLite TEXT timestamp representation used by stores.
func textTime(value string) time.Time { return sqlutil.ParseTime(value) }

// optionalTextTime preserves the legacy treatment of NULL and invalid timestamps.
func optionalTextTime(value *string) *time.Time {
	if value == nil {
		return nil
	}
	parsed := textTime(*value)
	if parsed.IsZero() {
		return nil
	}
	return &parsed
}

func textValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func timeText(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := sqlutil.FormatTime(*value)
	return &formatted
}

// mapRows preserves nil for an empty result, as the hand-written stores did.
func mapRows[R, D any](rows []R, fromRow func(R) D) []D {
	if len(rows) == 0 {
		return nil
	}
	out := make([]D, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row))
	}
	return out
}
