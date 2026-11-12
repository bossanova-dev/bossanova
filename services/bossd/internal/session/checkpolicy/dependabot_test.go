package checkpolicy

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestDependabotPolicyPaths_Sessions_AC248302(t *testing.T) {
	p := ParseDependabot([]byte(`{"ci":{"dependabot":{"required":[{"name":"always"},{"name":"lock","paths":["pnpm-lock.yaml","**/package.json"]},{"name":"go","paths":["services/?/go.*"]}]}}}`))
	if p.Malformed != nil || !p.Enabled {
		t.Fatalf("policy: %+v", p)
	}
	for _, tc := range []struct {
		file string
		want []string
	}{
		{"README.md", []string{"always"}},
		{"package.json", []string{"always", "lock"}},
		{"web/app/package.json", []string{"always", "lock"}},
		{"web/app\nlib/package.json", []string{"always", "lock"}},
		{"web/\n/package.json", []string{"always", "lock"}},
		{"pnpm-lock.yaml", []string{"always", "lock"}},
		{"services/a/go.mod", []string{"always", "go"}},
		{"services/ab/go.mod", []string{"always"}},
		{"xpnpm-lock.yaml", []string{"always"}},
	} {
		if got := p.RequiredFor([]string{tc.file}); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.file, got, tc.want)
		}
	}
}

func TestParseDependabotMalformed_Sessions_AC248305(t *testing.T) {
	for _, body := range []string{`{"ci":{"dependabot":null}}`, `{"ci":{"dependabot":true}}`, `{"ci":{"dependabot":{"required":null}}}`, `{"ci":{"dependabot":{"required":[{"name":" "}]}}}`, `{"ci":{"dependabot":{"required":[{"name":"ci","paths":["!x"]}]}}}`, `{"ci":{"dependabot":{"extra":1}}}`, `{"ci":{"dependabot":{"required":[{"name":"ci","paths":null}]}}}`, `{bad`} {
		p := ParseDependabot([]byte(body))
		if !p.Enabled || p.Malformed == nil {
			t.Errorf("accepted %s: %+v", body, p)
		}
	}
	for _, body := range []string{`{}`, `{"ci":{}}`, `{"ci":42}`, `{"ci":[]}`, `{"ci":"invalid"}`, `{"ci":null}`, `[]`, `42`, `"invalid"`, `true`} {
		p := ParseDependabot([]byte(body))
		if p.Enabled || p.Malformed != nil {
			t.Errorf("absent: %+v", p)
		}
	}
}

func TestResolverDependabotCache_Sessions_AC248305(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"ci":{"dependabot":false}}`)
	var logs bytes.Buffer
	r := New(policyRepoTable{}, zerolog.New(&logs))
	for range 3 {
		if p := r.DependabotPolicy(context.Background(), dir); p.Malformed == nil {
			t.Fatal("malformed allowed")
		}
	}
	if n := strings.Count(logs.String(), "malformed ci.dependabot"); n != 1 {
		t.Fatalf("warnings %d: %s", n, logs.String())
	}
	writeConfig(t, dir, `{"ci":{"required":{"checks":["ci"]},"dependabot":{"required":[{"name":"lock"}]}}}`)
	bumpModTime(t, dir)
	if p := r.DependabotPolicy(context.Background(), dir); !reflect.DeepEqual(p.RequiredFor(nil), []string{"lock"}) {
		t.Fatalf("reload: %+v", p)
	}
	if p := r.RequiredChecks(context.Background(), dir); !reflect.DeepEqual(p.Names, []string{"ci"}) {
		t.Fatalf("required: %+v", p)
	}
}
