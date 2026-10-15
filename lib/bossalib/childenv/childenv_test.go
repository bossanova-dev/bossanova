package childenv

import (
	"reflect"
	"testing"
)

func TestBuildBaseline(t *testing.T) {
	allowed := []string{"PATH=/bin", "HOME=/home/user", "USER=u", "LOGNAME=u", "SHELL=/bin/sh", "TMPDIR=/tmp", "TERM=xterm", "TZ=UTC", "LANG=C", "LC_ALL=C", "XDG_DATA_HOME=/data", "SSH_AUTH_SOCK=/sock", "HTTP_PROXY=http://proxy", "HTTPS_PROXY=https://proxy", "NO_PROXY=localhost", "ALL_PROXY=socks://proxy", "http_proxy=http://proxy", "https_proxy=https://proxy", "no_proxy=localhost", "all_proxy=socks://proxy", "SSL_CERT_FILE=/ca", "SSL_CERT_DIR=/certs", "NODE_EXTRA_CA_CERTS=/ca", "REQUESTS_CA_BUNDLE=/ca", "GH_TOKEN=token", "GITHUB_TOKEN=token", "GH_HOST=github.example", "BOSS_SETTINGS_PATH=/settings", "BOSSD_DAEMON_ID=id", "BOSSANOVA_PLUGIN=cookie", "BOSSANOVA_PLUGIN_PARENT_PID=123"}
	environ := append(append([]string{}, allowed...), "SOME_SECRET=private", "AWS_SECRET_ACCESS_KEY=private", "PATHETIC=private", "LC=private", "PATH", "=invalid")
	got := Build(environ, ForPlugin("repair", nil))
	if !reflect.DeepEqual(got, allowed) {
		t.Fatalf("Build() = %v, want approved entries %v", got, allowed)
	}
}

func TestForPluginProviderIsolation(t *testing.T) {
	tests := []struct {
		plugin          string
		allowed, denied []string
	}{
		{"claude", []string{"ANTHROPIC_API_KEY=x", "CLAUDE_CODE_OAUTH_TOKEN=x", "AWS_SECRET_ACCESS_KEY=x", "CLOUD_ML_REGION=x", "GOOGLE_APPLICATION_CREDENTIALS=x", "VERTEX_REGION_CLAUDE_SONNET=x"}, []string{"OPENAI_API_KEY=x", "OPENCODE_API_KEY=x", "SOME_SECRET=x"}},
		{"codex", []string{"OPENAI_API_KEY=x", "CODEX_HOME=x", "AZURE_OPENAI_API_KEY=x"}, []string{"AWS_SECRET_ACCESS_KEY=x", "ANTHROPIC_API_KEY=x"}},
		{"opencode", []string{"OPENCODE_DB=x", "ANTHROPIC_API_KEY=x", "OPENAI_API_KEY=x", "AWS_SECRET_ACCESS_KEY=x", "AZURE_RESOURCE_NAME=x", "CLOUDFLARE_API_TOKEN=x", "GOOGLE_APPLICATION_CREDENTIALS=x", "NVIDIA_API_KEY=x", "SNOWFLAKE_CORTEX_TOKEN=x", "XAI_API_KEY=x"}, []string{"AWS_UNKNOWN_SECRET=x", "SOME_SECRET=x"}},
		{"linear", []string{"LINEAR_API_ENDPOINT=x"}, []string{"ANTHROPIC_API_KEY=x", "LINEAR_API_KEY=x"}},
		{"unknown", nil, []string{"ANTHROPIC_API_KEY=x", "AWS_SECRET_ACCESS_KEY=x", "OPENAI_API_KEY=x", "OPENCODE_DB=x"}},
	}
	for _, tt := range tests {
		t.Run(tt.plugin, func(t *testing.T) {
			environ := append(append([]string{}, tt.allowed...), tt.denied...)
			got := Build(environ, ForPlugin(tt.plugin, nil))
			if len(got) != len(tt.allowed) || !reflect.DeepEqual(got, append([]string{}, tt.allowed...)) {
				t.Fatalf("Build() = %v, want %v", got, tt.allowed)
			}
		})
	}
}

func TestPassthroughNamesAndPrefixes(t *testing.T) {
	environ := []string{"CUSTOM_SECRET=value=with=equals", "MY_PROVIDER_TOKEN=x", "CUSTOM_SECRET_OTHER=x", "MY_PROVIDER=x", "OTHER=x"}
	got := Build(environ, ForPlugin("repair", []string{" CUSTOM_SECRET ", "MY_PROVIDER_*", "*", ""}))
	want := environ[:2]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Build() = %v, want %v", got, want)
	}
}

func TestBuildUsesOnlySuppliedPolicy(t *testing.T) {
	got := Build([]string{"PATH=/bin", "CUSTOM=x", "PREFIX_TOKEN=x"}, Policy{Names: []string{"CUSTOM"}, Prefixes: []string{"PREFIX_"}})
	want := []string{"CUSTOM=x", "PREFIX_TOKEN=x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Build() = %v, want %v", got, want)
	}
}
