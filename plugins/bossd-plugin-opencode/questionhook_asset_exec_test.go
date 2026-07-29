package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// jsDriver drives the SHIPPED question-hook asset through a real JavaScript
// runtime against a stub HTTP server, then prints the captured requests as
// JSON on stdout.
//
// This is the only test that EXECUTES the asset. The sibling
// TestEmbeddedQuestionHookAsset asserts on its source text, which cannot catch
// a syntax error, a wrong export shape, a wrong property path, or a wrong HTTP
// shape — and the asset is embedded, so any of those would fail silently at
// opencode plugin-load time in production, exactly where nobody is watching.
//
// The asset is copied to a .mjs file so node parses it as ESM regardless of the
// nearest package.json "type" (the repo root's is commonjs).
const jsDriver = `import http from 'node:http'
import { BossdQuestion } from './hook.mjs'

const captured = []
const server = http.createServer((req, res) => {
  let body = ''
  req.on('data', (c) => { body += c })
  req.on('end', () => {
    captured.push({
      url: req.url,
      method: req.method,
      auth: req.headers.authorization ?? '',
      contentType: req.headers['content-type'] ?? '',
      body,
    })
    res.writeHead(200)
    res.end()
  })
})
await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))

process.env.BOSS_HOOK_PORT = String(server.address().port)
process.env.BOSS_HOOK_TOKEN = 'tok-secret'

const hooks = await BossdQuestion({})
const fire = (type, properties) => hooks.event({ event: { type, properties } })

await fire('permission.asked', { sessionID: 'ses_main' })
await fire('question.asked', { sessionID: 'ses_main' })
await fire('session.idle', { sessionID: 'ses_main', info: { id: 'ses_main' } })
// Sub-agent suppression, both payload shapes.
await fire('permission.asked', { sessionID: 'ses_kid', info: { id: 'ses_kid', parentID: 'ses_main' } })
await fire('permission.asked', { sessionID: 'ses_kid2', parentID: 'ses_main' })
// Unhandled type, and a payload with no session id at all: both must be silent.
await fire('message.updated', { sessionID: 'ses_main' })
await fire('permission.asked', {})

// With no token in the env the hook must be wholly inert.
delete process.env.BOSS_HOOK_TOKEN
const inert = await BossdQuestion({})
await inert.event({ event: { type: 'permission.asked', properties: { sessionID: 'ses_main' } } })

console.log(JSON.stringify(captured))
process.exit(0)
`

type capturedRequest struct {
	URL         string `json:"url"`
	Method      string `json:"method"`
	Auth        string `json:"auth"`
	ContentType string `json:"contentType"`
	Body        string `json:"body"`
}

// TestEmbeddedQuestionHookAssetExecutes runs the embedded JS for real and pins
// the wire contract it must satisfy against BOS-485's receiver: the URL shape,
// the Bearer header, the JSON bodies for the question and clear arms, sub-agent
// suppression, and silence for everything else. It is the executable half of
// the plan's "simulated opencode event payload drives the plugin to POST the
// correct endpoint with the token" testing item.
// This test needs a JavaScript runtime and SKIPS without one. A Bazel action
// gets a minimal environment, so the go_test target inherits HOME and PATH
// (see BUILD.bazel's env_inherit) purely so this test really executes under
// `bazel test` — which is what `make test` and CI drive — rather than skipping
// there and leaving the shipped asset with no executable coverage on the gate.
// Removing that env_inherit turns this test into a silent no-op; it still needs
// no sandbox exemption.
func TestEmbeddedQuestionHookAssetExecutes(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping JS execution test (the source-text assertions in TestEmbeddedQuestionHookAsset still run)")
	}

	dir := t.TempDir()
	// The shipped bytes, not a copy of the source file: this test must fail if
	// the embedded asset drifts from what the runtime can actually run.
	if err := os.WriteFile(filepath.Join(dir, "hook.mjs"), questionHookJS, 0o600); err != nil {
		t.Fatalf("write hook.mjs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(jsDriver), 0o600); err != nil {
		t.Fatalf("write driver.mjs: %v", err)
	}

	cmd := exec.Command(node, "driver.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}

	var got []capturedRequest
	if err := json.Unmarshal(lastLine(out), &got); err != nil {
		t.Fatalf("parse driver output: %v\noutput:\n%s", err, out)
	}

	// permission.asked, question.asked, session.idle — and nothing else. A
	// longer slice means suppression leaked; a shorter one means a live arm
	// went silent.
	if len(got) != 3 {
		t.Fatalf("captured %d requests, want 3 (permission.asked, question.asked, session.idle): %+v", len(got), got)
	}

	wantBodies := []string{
		`{"notification_type":"permission_prompt","message":"opencode is waiting for a human response (permission.asked)"}`,
		`{"notification_type":"permission_prompt","message":"opencode is waiting for a human response (question.asked)"}`,
		`{"notification_type":"session_idle","cleared":true}`,
	}
	for i, req := range got {
		if req.Method != "POST" {
			t.Errorf("request %d method = %q, want POST", i, req.Method)
		}
		if req.URL != "/hooks/question/ses_main" {
			t.Errorf("request %d url = %q, want /hooks/question/ses_main", i, req.URL)
		}
		if req.Auth != "Bearer tok-secret" {
			t.Errorf("request %d authorization = %q, want the Bearer token from the env", i, req.Auth)
		}
		if req.ContentType != "application/json" {
			t.Errorf("request %d content-type = %q, want application/json", i, req.ContentType)
		}
		if req.Body != wantBodies[i] {
			t.Errorf("request %d body = %s, want %s", i, req.Body, wantBodies[i])
		}
	}

	// The clear arm is the ONLY one that may carry cleared:true — the receiver
	// gates its Clear on exactly that flag (clearingNotification), so a
	// question-arm body that grew the flag would retract the signal it just set.
	for i, req := range got[:2] {
		var body map[string]any
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			t.Fatalf("request %d body is not JSON: %v", i, err)
		}
		if _, ok := body["cleared"]; ok {
			t.Errorf("request %d (question arm) carries a cleared flag: %s", i, req.Body)
		}
	}
}

// lastLine returns the final non-empty line of the driver's output, so an
// incidental runtime warning on stderr cannot break the JSON parse.
func lastLine(out []byte) []byte {
	end := len(out)
	for end > 0 && (out[end-1] == '\n' || out[end-1] == '\r') {
		end--
	}
	start := end
	for start > 0 && out[start-1] != '\n' {
		start--
	}
	return out[start:end]
}
