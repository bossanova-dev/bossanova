package config

import (
	"testing"
)

func TestEnvOr(t *testing.T) {
	const key = "BOSSALIB_TEST_ENVVAR"

	t.Run("returns env value when set", func(t *testing.T) {
		t.Setenv(key, "from-env")

		if got := EnvOr(key, "fallback"); got != "from-env" {
			t.Errorf("EnvOr(%q, %q) = %q, want %q", key, "fallback", got, "from-env")
		}
	})

	t.Run("returns fallback when unset", func(t *testing.T) {
		// key is not set by default in sub-tests.
		if got := EnvOr(key, "fallback"); got != "fallback" {
			t.Errorf("EnvOr(%q, %q) = %q, want %q", key, "fallback", got, "fallback")
		}
	})

	t.Run("returns fallback when empty", func(t *testing.T) {
		t.Setenv(key, "")

		if got := EnvOr(key, "fallback"); got != "fallback" {
			t.Errorf("EnvOr(%q, %q) = %q, want %q", key, "fallback", got, "fallback")
		}
	})
}
