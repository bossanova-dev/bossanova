package ghusage

import (
	"net/url"
	"slices"
	"strings"
)

// ReadEnv contains the environment gh uses to resolve its host and repository.
type ReadEnv struct{ GHHost, GHRepo string }

// ReadRoute identifies a single-repository read. An empty Repo uses the working directory.
type ReadRoute struct {
	Routable bool
	Repo     string
	Reason   string
}

// ClassifyRead fails closed when gh's target or read semantics cannot be established.
func ClassifyRead(args []string, env ReadEnv) ReadRoute {
	if env.GHHost != "" && env.GHHost != "github.com" {
		return readRefusal("other_host")
	}
	for _, arg := range args {
		if arg == "--hostname" || strings.HasPrefix(arg, "--hostname=") {
			return readRefusal("other_host")
		}
	}
	if len(args) < 2 {
		return readRefusal("not_read")
	}
	if args[0] == "api" {
		return classifyAPIRead(args[1:], env)
	}
	if (args[0] != "pr" || !slices.Contains([]string{"view", "checks", "list", "diff"}, args[1])) &&
		(args[0] != "run" || (args[1] != "list" && args[1] != "view")) {
		return readRefusal("not_read")
	}
	return classifyCommandRead(args, env)
}

func classifyCommandRead(args []string, env ReadEnv) ReadRoute {
	command := args[0] + " " + args[1]
	repo, positional, reason := readCommandTarget(args[2:], command)
	if reason != "" {
		return readRefusal(reason)
	}
	if args[1] == "list" && len(positional) != 0 {
		return readRefusal("not_read")
	}
	urlRepo, reason := positionalReadRepo(positional)
	if reason != "" {
		return readRefusal(reason)
	}
	// gh prioritizes a PR URL over --repo; conflicts keep the user login.
	if urlRepo != "" {
		if repo != "" {
			explicit := routeRepository(repo)
			if !explicit.Routable {
				return explicit
			}
			if !strings.EqualFold(explicit.Repo, urlRepo) {
				return readRefusal("not_single_repo")
			}
		}
		repo = urlRepo
	}
	if repo == "" {
		repo = env.GHRepo
	}
	return routeRepository(repo)
}

func positionalReadRepo(args []string) (string, string) {
	repo := ""
	for _, arg := range args {
		if !strings.Contains(arg, "://") {
			continue
		}
		u, err := url.Parse(arg)
		if err != nil || u.Host != "github.com" {
			return "", "other_host"
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if u.Scheme != "https" || len(parts) != 4 || parts[2] != "pull" {
			return "", "not_single_repo"
		}
		repo = parts[0] + "/" + parts[1]
	}
	return repo, ""
}

func readRefusal(reason string) ReadRoute { return ReadRoute{Reason: reason} }

func routeRepository(repo string) ReadRoute {
	if repo == "" || repo == "{owner}/{repo}" {
		return ReadRoute{Routable: true, Reason: "installation"}
	}
	parts := strings.Split(repo, "/")
	if len(parts) == 3 {
		if parts[0] != "github.com" {
			return readRefusal("other_host")
		}
		parts = parts[1:]
	}
	if len(parts) != 2 || !readSlugPart(parts[0]) || !readSlugPart(parts[1]) {
		return readRefusal("not_single_repo")
	}
	return ReadRoute{Routable: true, Repo: strings.Join(parts, "/"), Reason: "installation"}
}

func readSlugPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func splitReadFlag(arg string) (string, string, bool) {
	if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
		return arg[:2], strings.TrimPrefix(arg[2:], "="), true
	}
	if key, val, ok := strings.Cut(arg, "="); ok {
		return key, val, true
	}
	return arg, "", false
}

func readCommandTarget(args []string, command string) (string, []string, string) {
	repo := ""
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flag, value, attached := splitReadFlag(arg)
		if flag == "--web" || command != "run list" && flag == "-w" || command == "pr checks" && flag == "--watch" {
			return "", nil, "not_read"
		}
		takesValue, known := readCommandFlag(command, flag)
		if !known {
			return "", nil, "not_read"
		}
		if !takesValue {
			if attached && (!strings.Contains(arg, "=") || value != "true" && value != "false") {
				return "", nil, "not_read"
			}
			continue
		}
		if !attached {
			i++
			if i >= len(args) {
				return "", nil, "not_read"
			}
			value = args[i]
		}
		if flag == "-R" || flag == "--repo" {
			repo = value
		}
	}
	return repo, positional, ""
}

type readCommandFlagSet struct{ values, switches []string }

var readCommandFlags = map[string]readCommandFlagSet{
	"pr view":   {switches: []string{"-c", "--comments"}},
	"pr checks": {values: []string{"-i", "--interval"}, switches: []string{"--required", "--fail-fast"}},
	"pr list":   {values: []string{"--app", "-a", "--assignee", "-A", "--author", "-B", "--base", "-H", "--head", "-l", "--label", "-L", "--limit", "-S", "--search", "-s", "--state"}, switches: []string{"-d", "--draft"}},
	"pr diff":   {values: []string{"--color", "-e", "--exclude"}, switches: []string{"--name-only", "--patch", "--allow-escape-sequences"}},
	"run list":  {values: []string{"-b", "--branch", "-c", "--commit", "--created", "-e", "--event", "-L", "--limit", "-s", "--status", "-u", "--user", "-w", "--workflow"}, switches: []string{"-a", "--all"}},
	"run view":  {values: []string{"-a", "--attempt", "-j", "--job"}, switches: []string{"--exit-status", "--log", "--log-failed", "-v", "--verbose"}},
}

func readCommandFlag(command, flag string) (bool, bool) {
	if flag == "-R" || flag == "--repo" {
		return true, true
	}
	if command != "pr diff" && slices.Contains([]string{"--json", "-q", "--jq", "-t", "--template"}, flag) {
		return true, true
	}
	flags := readCommandFlags[command]
	if slices.Contains(flags.values, flag) {
		return true, true
	}
	return false, slices.Contains(flags.switches, flag)
}

type readAPICall struct {
	method, path string
	fields       map[string]string
	hasInput     bool
}

func parseReadAPI(args []string) (readAPICall, bool) {
	call := readAPICall{fields: map[string]string{}}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			if call.path != "" {
				return call, false
			}
			call.path = args[i]
			continue
		}
		flag, value, attached := splitReadFlag(args[i])
		switch flag {
		case "--paginate", "--slurp", "--silent", "--include", "-i", "--verbose":
			continue
		case "-X", "--method", "-f", "-F", "--field", "--raw-field", "--input", "-H", "--header", "--jq", "-q", "--template", "-t", "--cache":
			if !attached {
				i++
				if i >= len(args) {
					return call, false
				}
				value = args[i]
			}
			if !call.setFlag(flag, value) {
				return call, false
			}
		default:
			return call, false
		}
	}
	return call, true
}

func (call *readAPICall) setFlag(flag, value string) bool {
	switch flag {
	case "-X", "--method":
		call.method = value
	case "--input":
		call.hasInput = true
	case "-f", "-F", "--field", "--raw-field":
		key, val, ok := strings.Cut(value, "=")
		if !ok {
			return false
		}
		call.fields[key] = val
	}
	return true
}

func classifyAPIRead(args []string, env ReadEnv) ReadRoute {
	call, ok := parseReadAPI(args)
	if !ok {
		return readRefusal("not_read")
	}
	method, path, fields := call.method, call.path, call.fields
	if call.hasInput {
		return readRefusal("not_read")
	}
	if path == "graphql" {
		if method != "" && method != "GET" && method != "POST" {
			return readRefusal("not_read")
		}
		return classifyGraphQLRead(fields, env)
	}
	if method == "" {
		method = "GET"
		if len(fields) > 0 {
			method = "POST"
		}
	}
	if method != "GET" {
		return readRefusal("not_read")
	}
	return classifyRESTRead(path, env)
}

func classifyRESTRead(path string, env ReadEnv) ReadRoute {
	path, _, _ = strings.Cut(strings.TrimPrefix(path, "/"), "?")
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[0] != "repos" {
		return readRefusal("not_single_repo")
	}
	for _, part := range parts {
		if part == "." || part == ".." || strings.Contains(part, "%") {
			return readRefusal("not_single_repo")
		}
	}
	if len(parts) > 3 && !slices.Contains([]string{"pulls", "issues", "commits", "git", "contents", "compare", "branches", "check-runs", "check-suites", "statuses", "actions", "readme", "tags", "releases", "labels", "milestones", "languages"}, parts[3]) {
		return readRefusal("not_single_repo")
	}
	repo := parts[1] + "/" + parts[2]
	if repo == "{owner}/{repo}" {
		repo = env.GHRepo
	}
	return routeRepository(repo)
}
