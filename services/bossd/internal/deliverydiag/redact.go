// Package deliverydiag renders body-free, bounded diagnostics for workers that
// persist delivery transport failures.
package deliverydiag

import (
	"sort"
	"strconv"
	"strings"
)

// MaxLength bounds what a single delivery failure may persist as a diagnostic.
const MaxLength = 512

const redactionSentinel = "\x00"

// Redact removes every raw or Go-quoted representation of body from cause
// before returning a bounded diagnostic. marker names the package-specific
// replacement visible to operators.
func Redact(cause error, body, marker string) string {
	text := ""
	if cause != nil {
		text = cause.Error()
	}

	needles := bodyRedactionNeedles(body)
	sort.Slice(needles, func(i, j int) bool { return len(needles[i]) > len(needles[j]) })
	for _, needle := range needles {
		text = strings.ReplaceAll(text, needle, redactionSentinel)
	}
	text = strings.ReplaceAll(text, redactionSentinel, marker)

	text = strings.TrimSpace(text)
	if text == "" {
		return "delivery failed"
	}
	if len(text) > MaxLength {
		suffix := "…"
		if strings.Contains(text, marker) {
			suffix = "\n" + marker + suffix
		}
		return strings.ToValidUTF8(text[:MaxLength-len(suffix)], "") + suffix
	}
	return text
}

func bodyRedactionNeedles(body string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, exists := seen[s]; exists {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, form := range []string{body, strings.TrimSpace(body)} {
		add(form)
		if quoted := strconv.Quote(form); len(quoted) >= 2 {
			add(quoted[1 : len(quoted)-1])
		}
	}
	return out
}
