package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	reasonNoConversationOpen   = "agy has not opened a conversation yet"
	reasonManyConversations    = "agy holds more than one conversation open"
	reasonNoRecentConversation = "no agy conversation for this directory started just after launch"
	reasonNoLaunchTime         = "no launch time to prove the conversation is this launch's"
)

// fallbackWindow bounds how long after the launch (or the chat's creation) a
// conversation found through last_conversations.json may have started. That
// file keeps only the newest conversation per directory, so without an upper
// bound a sibling chat's later conversation would match.
const fallbackWindow = 2 * time.Minute

// resolveInteractiveSession finds the conversation an interactive agy launch
// is using.
//
// agy creates the conversation on the first submitted input and keeps
// conversations/<id>.db open for as long as the process runs, idle included, so
// a live pane's process tree names its own conversation even when sibling chats
// share a worktree, and a live pane holding none has none yet. Only when there
// is no pane to ask does cache/last_conversations.json stand in, accepted when
// its first user turn falls within fallbackWindow of the launch (or, for legacy
// backfill, of the chat's creation).
func resolveInteractiveSession(store agyStore, procs processFiles, req *bossanovav1.ResolveInteractiveSessionIDRequest) *bossanovav1.ResolveInteractiveSessionIDResponse {
	notBefore := req.GetLaunchedAfter()
	if req.GetAllowLegacyBackfill() && req.GetChatCreatedAt() != nil {
		notBefore = req.GetChatCreatedAt()
	}
	if pid := int(req.GetPanePid()); pid > 0 {
		if files, err := procs.OpenFiles(pid); err == nil {
			ids := activeConversations(store, openConversations(store, files), notBefore)
			switch len(ids) {
			case 0:
				return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: reasonNoConversationOpen}
			case 1:
				path, _ := store.transcriptPath(ids[0])
				return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: ids[0], TranscriptPath: path}
			default:
				return &bossanovav1.ResolveInteractiveSessionIDResponse{Ambiguous: true, Reason: reasonManyConversations}
			}
		}
	}

	if notBefore == nil {
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: reasonNoLaunchTime}
	}
	id := store.lastConversation(req.GetWorkDir())
	path, ok := store.transcriptPath(id)
	if !ok {
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: reasonNoRecentConversation}
	}
	steps, err := readTranscriptSteps(path)
	if err != nil {
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: reasonNoRecentConversation}
	}
	first, ok := firstUserAt(steps)
	floor := secondFloor(notBefore)
	if !ok || first.Before(floor) || first.After(floor.Add(fallbackWindow)) {
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Reason: reasonNoRecentConversation}
	}
	return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: id, TranscriptPath: path}
}

// secondFloor truncates t to the second, the precision of agy's created_at.
func secondFloor(t *timestamppb.Timestamp) time.Time {
	return t.AsTime().Truncate(time.Second)
}

// activeConversations keeps the open conversations this launch is using. agy
// briefly opens other conversations' databases at startup (an empty one from
// another directory was seen), so an open file alone does not name the pane's
// conversation: it must also have a transcript written since notBefore. agy
// rewrites the transcript on every step and a passing open never touches it.
func activeConversations(store agyStore, ids []string, notBefore *timestamppb.Timestamp) []string {
	var active []string
	for _, id := range ids {
		path, ok := store.transcriptPath(id)
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		if notBefore != nil && info.ModTime().Before(secondFloor(notBefore)) {
			continue
		}
		active = append(active, id)
	}
	return active
}

func openConversations(store agyStore, files []string) []string {
	if store.root == "" {
		return nil
	}
	dirs := []string{store.conversationsDir()}
	if real, err := filepath.EvalSymlinks(store.conversationsDir()); err == nil {
		dirs = append(dirs, real)
	}
	var ids []string
	for _, f := range files {
		id, isDB := strings.CutSuffix(filepath.Base(f), ".db")
		if !isDB || !slices.Contains(dirs, filepath.Dir(f)) || !canonicalConversationID(id) || slices.Contains(ids, id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}
