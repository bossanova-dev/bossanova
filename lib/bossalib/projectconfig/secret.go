package projectconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
)

// secretEnvName is the only shape a secret reference may name.
var secretEnvName = regexp.MustCompile(`^BOSS_[A-Z0-9_]+$`)

// Environment variables export references the repo credentials by.
const (
	LinearAPIKeyEnv    = "BOSS_LINEAR_API_KEY"
	SentryAuthTokenEnv = "BOSS_SENTRY_AUTH_TOKEN"
)

// SecretRef references a secret by environment variable. The file can never
// carry a secret value: anything other than exactly {"env": "BOSS_…"} is a
// decode error, so a pasted credential cannot load.
type SecretRef struct {
	Env string `json:"env"`
}

// errSecretShape is the one message every malformed reference reports. It
// never includes the offending value, which may be a pasted credential.
var errSecretShape = errors.New(`must be exactly {"env": "BOSS_<NAME>"} naming a BOSS_* environment variable; secret values cannot appear in the file`)

// checkSecretRef validates the raw JSON shape of a reference.
func checkSecretRef(raw any) error {
	obj, ok := raw.(map[string]any)
	if !ok || len(obj) != 1 {
		return errSecretShape
	}
	env, ok := obj["env"].(string)
	if !ok || !secretEnvName.MatchString(env) {
		return errSecretShape
	}
	return nil
}

// UnmarshalJSON enforces the strict reference shape.
func (s *SecretRef) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return errSecretShape
	}
	if err := checkSecretRef(raw); err != nil {
		return err
	}
	obj, _ := raw.(map[string]any)
	s.Env, _ = obj["env"].(string)
	return nil
}

// SecretResolver resolves a reference to its value at apply time. A nil
// resolver means secrets are unresolvable (for example over MCP), and callers
// skip every secret change.
type SecretResolver interface {
	Resolve(ref SecretRef) (value string, ok bool)
}

// ResolveSecret resolves ref through r, treating a nil resolver as
// "unresolvable".
func ResolveSecret(r SecretResolver, ref SecretRef) (string, bool) {
	if r == nil {
		return "", false
	}
	return r.Resolve(ref)
}

type envResolver struct{ getenv func(string) string }

// EnvResolver resolves references from the environment through getenv
// (os.Getenv for the CLI). An unset or empty variable, or a name that is not
// BOSS_*, is unresolved.
func EnvResolver(getenv func(string) string) SecretResolver {
	return envResolver{getenv: getenv}
}

func (r envResolver) Resolve(ref SecretRef) (string, bool) {
	if r.getenv == nil || !secretEnvName.MatchString(ref.Env) {
		return "", false
	}
	v := r.getenv(ref.Env)
	return v, v != ""
}
