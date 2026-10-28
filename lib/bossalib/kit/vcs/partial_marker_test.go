package vcs

import (
	"encoding/json"
	"os"
	"testing"
)

func TestIsPartialMarked(t *testing.T) {
	data, err := os.ReadFile("testdata/partial_marker_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Title, Body string
		Partial           bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := IsPartialMarked(tc.Title, tc.Body); got != tc.Partial {
				t.Errorf("IsPartialMarked() = %v, want %v", got, tc.Partial)
			}
		})
	}
}
