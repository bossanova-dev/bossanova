//go:build !windows

package config

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeOwnerFileInfo struct {
	uid uint32
	sys any
}

func (f fakeOwnerFileInfo) Name() string       { return "fake" }
func (f fakeOwnerFileInfo) Size() int64        { return 1 }
func (f fakeOwnerFileInfo) Mode() os.FileMode  { return 0o755 }
func (f fakeOwnerFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeOwnerFileInfo) IsDir() bool        { return false }
func (f fakeOwnerFileInfo) Sys() any {
	if f.sys != nil {
		return f.sys
	}
	return &syscall.Stat_t{Uid: f.uid}
}

func TestOwnerTrustedAllowsCurrentUserAndRoot(t *testing.T) {
	tests := []struct {
		name string
		uid  uint32
	}{
		{name: "current user", uid: uint32(os.Getuid())},
		{name: "root", uid: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := ownerTrusted(fakeOwnerFileInfo{uid: tt.uid})
			if !ok {
				t.Fatalf("ownerTrusted() ok = false, reason = %q", reason)
			}
		})
	}
}

func TestOwnerTrustedRejectsOtherUser(t *testing.T) {
	otherUID := uint32(1)
	if otherUID == uint32(os.Getuid()) {
		otherUID = 2
	}

	ok, reason := ownerTrusted(fakeOwnerFileInfo{uid: otherUID})
	if ok {
		t.Fatalf("ownerTrusted() ok = true for uid %d, want false", otherUID)
	}
	if !strings.Contains(reason, "owned by uid") {
		t.Fatalf("ownerTrusted() reason = %q, want ownership detail", reason)
	}
}

func TestOwnerTrustedRejectsUnreadableOwnership(t *testing.T) {
	ok, reason := ownerTrusted(fakeOwnerFileInfo{sys: "not stat"})
	if ok {
		t.Fatal("ownerTrusted() ok = true for non-stat Sys, want false")
	}
	if reason != "cannot read file ownership" {
		t.Fatalf("ownerTrusted() reason = %q, want cannot read file ownership", reason)
	}
}
