package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// flagSuggestionMaxDistance bounds how far a rejected flag may be from a
// registered one before the suggestion is dropped. Two is cobra's own default
// for command-name suggestions (SuggestionsMinimumDistance), and every
// near-miss this hook exists for — `--tags` for the repeatable `--tag`,
// `--states` for `--state`, `--allowed-model` for `--allowed-models` — is a
// distance of one. Anything further apart is a different flag, not a typo, and
// naming it would be a worse answer than naming none.
const flagSuggestionMaxDistance = 2

// installFlagErrorHook makes every flag rejection under root say two things a
// bare `unknown flag: --tags` did not: which registered flag the caller
// probably meant, and — for a `--json` caller — that this was an invalid
// argument rather than the UNKNOWN an untagged local error resolves to.
//
// It is installed on the ROOT alone deliberately. cobra's FlagErrorFunc walks
// up to the parent when a command sets none (Command.FlagErrorFunc), and
// execute() applies it exactly once per parse, so one registration covers every
// subcommand — including the five repeatable/plural near-misses the enumeration
// found (`notes add|ls|edit --tag`, `ls --state`, `account update
// --allowed-models`) — without five special cases.
func installFlagErrorHook(root *cobra.Command) {
	root.SetFlagErrorFunc(flagErrorHook)
}

// flagErrorHook is the function installed above. Exported within the package so
// it can be unit-tested against a synthetic command tree.
func flagErrorHook(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	// `--help` reaches pflag as ErrHelp on a flag set that defines no help
	// flag. Cobra normally defines one, so this is defence in depth: help is
	// not a rejection and must never be reclassified as an invalid argument.
	if errors.Is(err, pflag.ErrHelp) {
		return err
	}
	// pflag 1.0.10 carries typed errors, so the flag name is read from the
	// error rather than scraped out of its message — the same reason this
	// package's envelope vocabulary exists instead of message matching.
	var notExist *pflag.NotExistError
	if errors.As(err, &notExist) {
		// A shorthand group (`-x` inside `-abc`) has no long-form name to
		// compare against, so there is nothing to suggest; it still classifies.
		if notExist.GetSpecifiedShortnames() == "" {
			if nearest, ok := nearestFlagName(cmd, notExist.GetSpecifiedName()); ok {
				return codedError(codeInvalidArgument, fmt.Errorf("%w; did you mean --%s?", err, nearest))
			}
		}
	}
	return codedError(codeInvalidArgument, err)
}

// nearestFlagName returns the registered flag on cmd closest to name, when one
// is within flagSuggestionMaxDistance. Both the command's own flags and the
// inherited persistent flags are considered: a caller who mistypes `--remote`
// is owed the same answer as one who mistypes a local flag.
//
// Hidden and deprecated flags are skipped. Suggesting a flag that `--help` does
// not list would send the caller looking for something they cannot find
// documented, which is a worse failure than no suggestion at all.
func nearestFlagName(cmd *cobra.Command, name string) (string, bool) {
	if cmd == nil || name == "" {
		return "", false
	}
	best := ""
	bestDistance := flagSuggestionMaxDistance + 1
	consider := func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" || f.Name == name {
			return
		}
		d := editDistance(name, f.Name)
		// Ties are broken by flag name so the suggestion is deterministic
		// rather than dependent on pflag's iteration order.
		if d < bestDistance || (d == bestDistance && best != "" && f.Name < best) {
			best, bestDistance = f.Name, d
		}
	}
	cmd.Flags().VisitAll(consider)
	cmd.InheritedFlags().VisitAll(consider)
	if bestDistance > flagSuggestionMaxDistance {
		return "", false
	}
	return best, true
}

// editDistance is the Levenshtein distance between a and b, compared over
// runes so a multi-byte flag name is not measured in bytes. Case-insensitive:
// flag names are lowercase by convention, and a caller who shouted one should
// still be pointed at it.
func editDistance(a, b string) int {
	ar := []rune(strings.ToLower(a))
	br := []rune(strings.ToLower(b))
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	// Single-row DP: only the previous row is ever read.
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = minInt(minInt(curr[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// registeredFlagNames lists the non-hidden, non-deprecated flag names on cmd,
// sorted. Used by the tests to prove the hook's candidate set is the command's
// real surface rather than a hard-coded list.
func registeredFlagNames(cmd *cobra.Command) []string {
	seen := map[string]struct{}{}
	collect := func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" {
			return
		}
		seen[f.Name] = struct{}{}
	}
	cmd.Flags().VisitAll(collect)
	cmd.InheritedFlags().VisitAll(collect)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
