package scenario

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

// OperationClient is the real daemon boundary used by the run-owned keeper.
type OperationClient interface {
	MergeSession(context.Context, string) error
	ArchiveSession(context.Context, string) (bool, error)
	ReportChatStatus(context.Context, string, pb.ChatStatus, time.Time) error
}

type heldOperation struct {
	Operation
	Name      string `json:"name"`
	SessionID string `json:"sessionId"`
	ChatID    string `json:"chatId,omitempty"`
}

type runningOperation struct {
	heldOperation
	done     <-chan struct{}
	result   chan error
	released bool
}

func operationName(op Operation) string {
	prefix := "merge"
	if op.Op == "holdArchive" {
		prefix = "archive"
	}
	return fmt.Sprintf("%s-%x", prefix, sha256.Sum256([]byte(op.Session)))[:len(prefix)+1+24]
}

func marker(home, name, suffix string) string { return filepath.Join(home, "holds", name+"."+suffix) }
func exists(path string) bool                 { _, err := os.Stat(path); return err == nil }
func mark(home, name, suffix string) error {
	return os.WriteFile(marker(home, name, suffix), nil, 0o600)
}

// RunOperations holds real daemon operations until ReleaseOperations signals them.
// ready runs only after every external boundary has been entered. The caller owns
// this blocking keeper's process and context; every merge worker is joined before return.
func RunOperations(ctx context.Context, doc Document, keys Keys, home string, client OperationClient, ready func() error) []Problem {
	held := newHeldOperations(doc, keys)
	if err := prepareHolds(home, held); err != nil {
		return []Problem{operationProblem(Operation{}, err)}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer stopOperations(home, client, cancel, held)
	for _, h := range held {
		if err := startOperation(runCtx, home, client, h); err != nil {
			return []Problem{operationProblem(h.Operation, err)}
		}
	}
	if err := ready(); err != nil {
		return []Problem{operationProblem(Operation{}, err)}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		remaining := len(held)
		for _, h := range held {
			released, err := tickOperation(runCtx, home, client, h)
			if err != nil {
				return []Problem{operationProblem(h.Operation, err)}
			}
			if released {
				remaining--
			}
		}
		if remaining == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return []Problem{operationProblem(Operation{}, ctx.Err())}
		case <-ticker.C:
		}
	}
}
func newHeldOperations(doc Document, keys Keys) []*runningOperation {
	held := make([]*runningOperation, 0, len(doc.Operations))
	for _, op := range doc.Operations {
		h := heldOperation{Operation: op, Name: operationName(op), SessionID: keys.Sessions[op.Session]}
		if op.Op == "holdArchive" {
			for _, s := range doc.Sessions {
				if s.Key == op.Session && len(s.Chats) > 0 {
					h.ChatID = archiveChatID(keys, s, s.Chats[0])
				}
			}
		}
		held = append(held, &runningOperation{heldOperation: h, result: make(chan error, 1)})
	}
	return held
}
func prepareHolds(home string, held []*runningOperation) error {
	if err := os.MkdirAll(filepath.Join(home, "holds"), 0o700); err != nil {
		return err
	}
	records := make([]heldOperation, 0, len(held))
	for _, h := range held {
		// A preceding apply released its holds. Never accept its entry marker as
		// readiness for a new invocation of the same scenario key.
		for _, suffix := range []string{"entered", "release", "released"} {
			if err := os.Remove(marker(home, h.Name, suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		records = append(records, h.heldOperation)
	}
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "scenario-holds.json"), data, 0o600)
}
func stopOperations(home string, client OperationClient, cancel context.CancelFunc, held []*runningOperation) {
	// Startup failures and cancellation release external boundaries before workers
	// are joined. Keep a short independent budget for the final idle reports.
	for _, h := range held {
		_ = mark(home, h.Name, "release")
	}
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	for _, h := range held {
		if h.ChatID != "" {
			_ = reportChat(ctx, client, h.ChatID, pb.ChatStatus_CHAT_STATUS_IDLE)
		}
	}
	cancel()
	for _, h := range held {
		if h.done != nil {
			<-h.done
		}
		_ = mark(home, h.Name, "released")
	}
}
func tickOperation(ctx context.Context, home string, client OperationClient, h *runningOperation) (bool, error) {
	if h.released {
		return true, nil
	}
	if !exists(marker(home, h.Name, "release")) {
		if h.ChatID != "" {
			if err := reportChat(ctx, client, h.ChatID, pb.ChatStatus_CHAT_STATUS_WORKING); err != nil {
				return false, err
			}
		}
		if h.done != nil {
			select {
			case <-h.done:
				return false, fmt.Errorf("merge ended before release: %v", <-h.result)
			default:
			}
		}
		return false, nil
	}
	if h.ChatID != "" {
		if err := reportChat(ctx, client, h.ChatID, pb.ChatStatus_CHAT_STATUS_IDLE); err != nil {
			return false, err
		}
	}
	if h.done != nil {
		select {
		case <-h.done:
		default:
			return false, nil
		}
	}
	h.released = true
	return true, mark(home, h.Name, "released")
}

func startOperation(ctx context.Context, home string, client OperationClient, h *runningOperation) error {
	if h.Op == "holdArchive" {
		if h.ChatID == "" {
			return fmt.Errorf("holdArchive requires a recorded scenario chat")
		}
		if err := reportChat(ctx, client, h.ChatID, pb.ChatStatus_CHAT_STATUS_WORKING); err != nil {
			return err
		}
		pending, err := client.ArchiveSession(ctx, h.SessionID)
		if err != nil {
			return err
		}
		if !pending {
			return fmt.Errorf("ArchiveSession did not defer: a live busy chat is required")
		}
		return nil
	}
	h.done = safego.Go(zerolog.Nop(), func() {
		err := client.MergeSession(ctx, h.SessionID)
		h.result <- err
	})
	enteredCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if exists(marker(home, h.Name, "entered")) {
			return nil
		}
		select {
		case <-h.done:
			return fmt.Errorf("MergeSession refused before entering fake gh: %v", <-h.result)
		case <-enteredCtx.Done():
			return fmt.Errorf("waiting for fake gh merge boundary: %w", enteredCtx.Err())
		case <-ticker.C:
		}
	}
}

func reportChat(ctx context.Context, client OperationClient, id string, status pb.ChatStatus) error {
	return client.ReportChatStatus(ctx, id, status, time.Now())
}
func operationProblem(op Operation, err error) Problem {
	return Problem{Code: "operation-refused", Path: "operations", Message: fmt.Sprintf("%s %s: %v", op.Op, op.Session, err), Hint: "holdMerge requires an open, non-draft, mergeable PR with passing checks and no requested changes; holdArchive requires a recorded scenario chat reporting working. Inspect bossd's reason and correct those inputs."}
}

// ReleaseOperations signals selected session keys, or all holds, and waits for the
// keeper to acknowledge stopping busy reports and completing merge requests.
func ReleaseOperations(ctx context.Context, home string, keys []string) ApplyResult {
	result := ApplyResult{OK: true, Operations: []Operation{}, Problems: []Problem{}}
	data, err := os.ReadFile(filepath.Join(home, "scenario-holds.json"))
	if os.IsNotExist(err) {
		return result
	}
	var held []heldOperation
	if err == nil {
		err = json.Unmarshal(data, &held)
	}
	if err != nil {
		result.OK = false
		result.Problems = []Problem{operationProblem(Operation{}, err)}
		return result
	}
	for _, key := range keys {
		if !slices.ContainsFunc(held, func(h heldOperation) bool { return h.Session == key }) {
			result.OK = false
			result.Problems = append(result.Problems, operationProblem(Operation{}, fmt.Errorf("unknown held session %q", key)))
		}
	}
	if !result.OK {
		return result
	}
	var selected []heldOperation
	for _, h := range held {
		if len(keys) > 0 && !slices.Contains(keys, h.Session) {
			continue
		}
		// Persisted names are never accepted as paths from the record.
		if h.Name != operationName(h.Operation) {
			result.OK = false
			result.Problems = append(result.Problems, operationProblem(h.Operation, fmt.Errorf("invalid hold marker")))
			return result
		}
		if err = mark(home, h.Name, "release"); err != nil {
			result.OK = false
			result.Problems = append(result.Problems, operationProblem(h.Operation, err))
			return result
		}
		result.Operations = append(result.Operations, h.Operation)
		selected = append(selected, h)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for _, h := range selected {
		for !exists(marker(home, h.Name, "released")) {
			select {
			case <-ctx.Done():
				result.OK = false
				result.Problems = append(result.Problems, operationProblem(h.Operation, ctx.Err()))
				return result
			case <-ticker.C:
			}
		}
	}
	return result
}

func archiveChatID(keys Keys, session Session, chat Chat) string {
	return fmt.Sprintf("scenario-%x", sha256.Sum256([]byte(keys.Repos[session.Repo]+"\x00"+session.Key+"\x00"+chat.Key)))
}
func holdsArchive(operations []Operation, key string) bool {
	for _, op := range operations {
		if op.Op == "holdArchive" && op.Session == key {
			return true
		}
	}
	return false
}
