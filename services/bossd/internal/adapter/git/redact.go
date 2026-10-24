package git

import (
	"strings"

	"github.com/recurser/bossd/internal/domain"
)

func redactURLCredentials(s string) string {
	return domain.RedactURLCredentials(s)
}

// RedactURLCredentials masks credentials in URLs within diagnostic text.
// For logging or error text; never pass its result to git or store it.
func RedactURLCredentials(s string) string {
	return domain.RedactURLCredentials(s)
}

// redactedGitArgv renders a git argv for an error or log line with URL credentials redacted.
func redactedGitArgv(args []string) string {
	return domain.RedactURLCredentials(strings.Join(args, " "))
}
