package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

// TestLogRotationLaneAvailability asserts the startup lane-availability
// diagnostic carries the proof token plus the seams, kill-switch, and per-repo
// CanAutoRotate counts in a single log line (BOS-315).
func TestLogRotationLaneAvailability(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	repos := []*models.Repo{
		{CanAutoRotate: true},
		{CanAutoRotate: true},
		{CanAutoRotate: false},
	}

	logRotationLaneAvailability(logger, true, false, repos, nil)

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want exactly one log line, got:\n%s", out)
	}
	for _, tok := range []string{
		"HasLiveRotationSeams",
		`"has_live_rotation_seams":true`,
		`"rotation_enabled":false`,
		`"auto_rotate_repos":2`,
		`"opted_out_repos":1`,
	} {
		if !strings.Contains(out, tok) {
			t.Fatalf("log missing token %q in:\n%s", tok, out)
		}
	}
}

// TestLogRotationLaneAvailability_FailSoft asserts a repo-list error still emits
// the seams + kill-switch fields with zero counts and never blocks startup.
func TestLogRotationLaneAvailability_FailSoft(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	logRotationLaneAvailability(logger, false, true, nil, errors.New("db unavailable"))

	out := buf.String()
	for _, tok := range []string{
		"HasLiveRotationSeams",
		`"has_live_rotation_seams":false`,
		`"rotation_enabled":true`,
		`"auto_rotate_repos":0`,
		`"opted_out_repos":0`,
		"repo_count_error",
	} {
		if !strings.Contains(out, tok) {
			t.Fatalf("fail-soft log missing token %q in:\n%s", tok, out)
		}
	}
}
