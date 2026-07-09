package session

import "strings"

// BossControlCommand is the parsed form of a single-line boss control directive
// that bossd intercepts before delivering a chat message to the agent. Today the
// only such directive is the manual account switch ("/boss switch" or "/switch").
//
// Account is the optional target account (id or label) named after the command
// word(s); it is empty when the user requested a switch without naming a target
// (meaning "pick a default other account"). Force is true when a "--force" flag
// appeared anywhere among the arguments, signalling that a mid-turn switch may
// interrupt the in-flight turn.
type BossControlCommand struct {
	Account string
	Force   bool
}

// ParseBossControlCommand reports whether message is a boss control directive and,
// if so, returns its parsed form. It is a pure function over the message text: no
// disk or skill lookups, because the recognised directives are fixed built-ins.
//
// It mirrors the detection idioms of chatInputMechanicsFromPrompt: the message is
// trimmed of surrounding whitespace, and any multi-line message (containing "\r"
// or "\n") is NEVER a control command — it is ordinary chat content and returns
// (nil, false).
//
// A directive matches only when the command word is in leading position:
//   - first token "/boss" followed by second token "switch", or
//   - first token "/switch".
//
// This deliberately rejects "/bossswitch", "/boss switcheroo", "/boss" alone,
// "/switchfoo", native built-ins like "/model", free-form prose, and any "/switch"
// that is not the leading token (e.g. "please /boss switch now").
//
// Among the tokens after the matched command word(s): a "--force" token in any
// position sets Force; the first non-flag token is the optional Account. A SECOND
// positional (non-flag) token is treated as invalid input and returns (nil, false)
// rather than silently truncating — so "/boss switch a b" is rejected. (The plan's
// Open Question permitted either a typed error or (nil, false); (nil, false) is
// chosen here to keep the signature pure.)
func ParseBossControlCommand(message string) (*BossControlCommand, bool) {
	trimmed := strings.TrimSpace(message)
	if strings.ContainsAny(trimmed, "\r\n") {
		return nil, false
	}

	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return nil, false
	}

	// Identify the command and the index at which arguments begin.
	var argStart int
	switch {
	case fields[0] == "/boss" && len(fields) >= 2 && fields[1] == "switch":
		argStart = 2
	case fields[0] == "/switch":
		argStart = 1
	default:
		return nil, false
	}

	cmd := &BossControlCommand{}
	positionalSeen := false
	for _, tok := range fields[argStart:] {
		if tok == "--force" {
			cmd.Force = true
			continue
		}
		if positionalSeen {
			// A second positional token is invalid: refuse rather than truncate.
			return nil, false
		}
		cmd.Account = tok
		positionalSeen = true
	}

	return cmd, true
}
