package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func apiResponseOutput(resp *http.Response, body []byte, include bool) string {
	if !include {
		return string(body)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s %s\r\n", resp.Proto, resp.Status)
	_ = resp.Header.Write(&out)
	out.WriteString("\r\n")
	out.Write(body)
	return out.String()
}

// gh api (unlike the typed GraphQL client) joins GraphQL error messages with
// newlines and prefixes the aggregate with "gh: "; it does not print paths.
// Source: https://github.com/cli/cli/blob/v2.83.1/pkg/cmd/api/api.go
func apiResponseError(status int, body []byte, graphql bool) string {
	if status >= 200 && status <= 299 && !graphql {
		return ""
	}
	var parsed struct {
		Message string
		Errors  json.RawMessage
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Message != "" {
		return fmt.Sprintf("gh: %s (HTTP %d)", parsed.Message, status)
	}
	if len(parsed.Errors) > 0 {
		var single string
		if json.Unmarshal(parsed.Errors, &single) == nil && single != "" {
			return "gh: " + single
		}
		var entries []json.RawMessage
		if json.Unmarshal(parsed.Errors, &entries) == nil {
			messages := make([]string, 0, len(entries))
			for _, entry := range entries {
				var object struct{ Message string }
				var message string
				if json.Unmarshal(entry, &object) == nil {
					message = object.Message
				} else {
					_ = json.Unmarshal(entry, &message)
				}
				if message != "" {
					messages = append(messages, message)
				}
			}
			if len(messages) > 0 {
				return "gh: " + strings.Join(messages, "\n")
			}
		}
	}
	if status > 299 {
		return fmt.Sprintf("gh: HTTP %d", status)
	}
	return ""
}
