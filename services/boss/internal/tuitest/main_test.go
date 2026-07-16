package tuitest_test

import (
	"os"
	"testing"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const waitTimeout = 10 * time.Second

func TestMain(m *testing.M) {
	cleanup := tuitest.BuildBoss()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func testRepos() []*pb.Repo {
	return fixtures.Repos()[:1] // legacy single-repo callers
}

func testMultiRepos() []*pb.Repo {
	return fixtures.Repos()
}

func testSessions() []*pb.Session {
	return fixtures.ActiveSessions()
}

func testChats() []*pb.ClaudeChat {
	return fixtures.Chats()
}
