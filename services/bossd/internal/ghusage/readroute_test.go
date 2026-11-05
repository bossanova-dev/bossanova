package ghusage

import "testing"

func TestClassifyRead(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		env          ReadEnv
		repo, reason string
	}{
		{"rest implicit", []string{"api", "repos/{owner}/{repo}/pulls"}, ReadEnv{}, "", "installation"},
		{"rest literal", []string{"api", "/repos/o/r/issues?state=open"}, ReadEnv{}, "o/r", "installation"},
		{"get fields", []string{"api", "-XGET", "-f", "per_page=100", "repos/o/r/pulls"}, ReadEnv{}, "o/r", "installation"},
		{"attached fields", []string{"api", "-X=GET", "-fper_page=100", "repos/o/r/pulls"}, ReadEnv{}, "o/r", "installation"},
		{"post fields", []string{"api", "-f", "title=x", "repos/o/r/issues"}, ReadEnv{}, "", "not_read"},
		{"input", []string{"api", "repos/o/r/issues", "--input=-", "-XGET"}, ReadEnv{}, "", "not_read"},
		{"patch", []string{"api", "repos/o/r/issues/1", "-X", "PATCH"}, ReadEnv{}, "", "not_read"},
		{"user", []string{"api", "user"}, ReadEnv{}, "", "not_single_repo"},
		{"search", []string{"api", "search/issues"}, ReadEnv{}, "", "not_single_repo"},
		{"unknown resource", []string{"api", "repos/o/r/collaborators"}, ReadEnv{}, "", "not_single_repo"},
		{"traversal", []string{"api", "repos/o/r/issues/../../hooks"}, ReadEnv{}, "", "not_single_repo"},
		{"pr", []string{"pr", "view", "12"}, ReadEnv{}, "", "installation"},
		{"url", []string{"pr", "view", "https://github.com/o/r/pull/12"}, ReadEnv{}, "o/r", "installation"},
		{"explicit", []string{"pr", "list", "-R", "o/r"}, ReadEnv{}, "o/r", "installation"},
		{"comments before repo", []string{"pr", "view", "-c", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"comments before URL", []string{"pr", "view", "-c", "https://github.com/acme/other/pull/1"}, ReadEnv{}, "acme/other", "installation"},
		{"all runs before repo", []string{"run", "list", "-a", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"all runs before URL", []string{"run", "list", "-a", "https://github.com/acme/other/pull/1"}, ReadEnv{}, "", "not_read"},
		{"run attempt value", []string{"run", "view", "-a", "2", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"run commit value", []string{"run", "list", "-c", "deadbeef", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"PR assignee value", []string{"pr", "list", "-a", "alice", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"draft boolean", []string{"pr", "list", "-d", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"checks interval value", []string{"pr", "checks", "-i", "10", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"diff color value", []string{"pr", "diff", "--color", "always", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"diff excludes value", []string{"pr", "diff", "-e", "https://example.invalid/files", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"run verbose boolean", []string{"run", "view", "-v", "-R", "acme/other"}, ReadEnv{}, "acme/other", "installation"},
		{"wrong command flag", []string{"run", "list", "--comments", "-R", "acme/other"}, ReadEnv{}, "", "not_read"},
		{"ambiguous boolean cluster", []string{"run", "list", "-aRacme/other"}, ReadEnv{}, "", "not_read"},
		{"conflicting PR selectors", []string{"pr", "view", "https://github.com/x/other/pull/1", "-R", "acme/app"}, ReadEnv{}, "", "not_single_repo"},
		{"comments conflicting selectors", []string{"pr", "view", "-c", "-R", "acme/app", "https://github.com/x/other/pull/1"}, ReadEnv{}, "", "not_single_repo"},
		{"cross host URL with repo", []string{"pr", "checks", "https://ghe.example/x/other/pull/1", "-R", "acme/app"}, ReadEnv{}, "", "other_host"},
		{"matching explicit URL", []string{"pr", "diff", "https://github.com/acme/app/pull/1", "-R", "github.com/ACME/app"}, ReadEnv{}, "acme/app", "installation"},
		{"env repo", []string{"pr", "diff", "12"}, ReadEnv{GHRepo: "o/r"}, "o/r", "installation"},
		{"checks", []string{"pr", "checks", "12"}, ReadEnv{}, "", "installation"},
		{"run list", []string{"run", "list"}, ReadEnv{}, "", "installation"},
		{"run workflow", []string{"run", "list", "-w", "ci.yml"}, ReadEnv{}, "", "installation"},
		{"web short", []string{"pr", "view", "-w"}, ReadEnv{}, "", "not_read"},
		{"run log", []string{"run", "view", "99", "--log"}, ReadEnv{}, "", "installation"},
		{"watch", []string{"pr", "checks", "--watch"}, ReadEnv{}, "", "not_read"},
		{"web", []string{"pr", "view", "--web"}, ReadEnv{}, "", "not_read"},
		{"merge", []string{"pr", "merge", "1"}, ReadEnv{}, "", "not_read"},
		{"comment", []string{"pr", "comment", "1"}, ReadEnv{}, "", "not_read"},
		{"edit", []string{"pr", "edit", "1"}, ReadEnv{}, "", "not_read"},
		{"ready", []string{"pr", "ready", "1"}, ReadEnv{}, "", "not_read"},
		{"rerun", []string{"run", "rerun", "1"}, ReadEnv{}, "", "not_read"},
		{"run watch", []string{"run", "watch", "1"}, ReadEnv{}, "", "not_read"},
		{"host flag", []string{"api", "repos/o/r", "--hostname", "github.com"}, ReadEnv{}, "", "other_host"},
		{"host env", []string{"pr", "list"}, ReadEnv{GHHost: "ghe.example"}, "", "other_host"},
		{"host repo", []string{"pr", "list", "-Rghe.example/o/r"}, ReadEnv{}, "", "other_host"},
		{"empty", nil, ReadEnv{}, "", "not_read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRead(tt.args, tt.env)
			if got.Repo != tt.repo || got.Reason != tt.reason || got.Routable != (tt.reason == "installation") {
				t.Fatalf("ClassifyRead = %+v, want repo %q reason %q", got, tt.repo, tt.reason)
			}
		})
	}
}

func TestClassifyGraphQLRead(t *testing.T) {
	tests := []struct {
		name, query  string
		fields       []string
		repo, reason string
	}{
		{"literal", `{repository(owner:"o",name:"r"){name}}`, nil, "o/r", "installation"},
		{"variables", `query Q($owner:String!,$name:String!){repository(owner:$owner,name:$name){id}}`, []string{"owner=o", "name=r"}, "o/r", "installation"},
		{"placeholders", `{repository(owner:"{owner}",name:"{repo}"){id}}`, nil, "", "installation"},
		{"aliases", `{a:repository(owner:"o",name:"r"){id} b:repository(owner:"o",name:"r"){name}}`, nil, "o/r", "installation"},
		{"comments and strings", "# mutation\n{repository(owner:\"o\",name:\"r\"){issues(first:1,filterBy:{createdBy:\"mutation\"}){nodes{title}}}}", nil, "o/r", "installation"},
		{"mutation", `mutation {repository(owner:"o",name:"r"){id}}`, nil, "", "not_read"},
		{"subscription", `subscription {repository(owner:"o",name:"r"){id}}`, nil, "", "not_read"},
		{"viewer", `{viewer{login}}`, nil, "", "not_single_repo"},
		{"mixed root", `{repository(owner:"o",name:"r"){id} viewer{login}}`, nil, "", "not_single_repo"},
		{"two repos", `{a:repository(owner:"o",name:"r"){id} b:repository(owner:"o",name:"s"){id}}`, nil, "", "not_single_repo"},
		{"unbound", `{repository(owner:$owner,name:"r"){id}}`, nil, "", "not_single_repo"},
		{"fragments", `{... Repo} fragment Repo on Query {viewer{login}}`, nil, "", "not_single_repo"},
		{"malformed", `{repository(owner:"o",name:"r"){id}`, nil, "", "not_single_repo"},
		{"multiple operations", `query A{repository(owner:"o",name:"r"){id}} query B{viewer{login}}`, nil, "", "not_single_repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"api", "graphql", "-f", "query=" + tt.query}
			for _, field := range tt.fields {
				args = append(args, "-F", field)
			}
			got := ClassifyRead(args, ReadEnv{})
			if got.Repo != tt.repo || got.Reason != tt.reason || got.Routable != (tt.reason == "installation") {
				t.Fatalf("got %+v, want repo %q reason %q", got, tt.repo, tt.reason)
			}
		})
	}
}
