package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/bus"
)

// Remote approval over the repo session — the transport half of
// pkg/ask/remote.go.
//
// A human on host B answers an approval queued on host A by appending ONE
// TaskEvent{Kind: "approval"} to the same repo-session feed remote mail rides
// (SendApprovalDirective). A's next feed drain (DeliverRemoteMail, which every
// inbox pass runs) hands a directive addressed to its host to ask.ApplyRemote
// — the whole policy: allowed person, exact plan token, settle once — and
// appends an explicit TaskEvent{Kind: "approval-receipt"} back, applied or
// rejected. B reads that receipt with FindApprovalReceipt.
//
// No new transport and no cloudbox change: cloudbox validates only that a
// kind is non-empty (and at most 16 chars), and session membership on the
// paired account is what admits an append at all. The approval record, its
// decision and its audit never leave A.

// Task-event kinds; cloudbox caps a kind at 16 characters.
const (
	ApprovalDirectiveKind = "approval"
	ApprovalReceiptKind   = "approval-receipt"
)

// RemoteApproversEnv lists additional Cloudbox account emails allowed to
// answer this host's approvals. Seat names in event detail are untrusted.
const RemoteApproversEnv = "BASHY_REMOTE_APPROVERS"

func allowedRemoteAuthor(author, owner string) bool {
	if author == "" || !strings.Contains(author, "@") {
		return false
	}
	if owner != "" && strings.EqualFold(author, owner) {
		return true
	}
	for _, email := range strings.Split(os.Getenv(RemoteApproversEnv), ",") {
		email = strings.TrimSpace(email)
		if strings.Contains(email, "@") && strings.EqualFold(author, email) {
			return true
		}
	}
	return false
}

// bindApprovalAuthor checks the server-stamped account against the task owner
// (or an explicit account allowlist), then proves that this same account
// joined the session as the person seat named in the directive. Neither By nor
// ByURN can grant authority: both arrived in client-controlled event detail.
func bindApprovalAuthor(ctx context.Context, sc *sessionRepoClient, taskID string, ev Event, d *ask.RemoteDirective) string {
	author := strings.TrimSpace(ev.Author)
	if author == "" {
		return "approval event has no server-stamped author"
	}
	tasks, err := sc.client.ListTasks(ctx)
	if err != nil {
		return "cannot verify approval task owner: " + err.Error()
	}
	owner := ""
	for _, task := range tasks {
		if task.ID == taskID {
			owner = strings.TrimSpace(task.Owner)
			break
		}
	}
	if owner == "" || !allowedRemoteAuthor(author, owner) {
		return "approval author is not an allowed account"
	}
	if !strings.HasPrefix(d.By, "person:") || !strings.Contains(d.By, "@") {
		return "approval directive has no person seat"
	}
	roster, err := sc.client.GetEvents(ctx, taskID, "", 0)
	if err != nil {
		return "cannot verify approval roster: " + err.Error()
	}
	joined := false
	for _, presence := range roster.Events {
		if !strings.EqualFold(presence.Author, author) || !strings.EqualFold(joinParticipant(presence), d.By) {
			continue
		}
		switch presence.Kind {
		case "join":
			joined = true
		case "leave":
			joined = false
		}
	}
	if !joined {
		return "approval author has no active join for the claimed person seat"
	}
	d.ByURN = author // audit the account proved by cloudbox, never payload ByURN
	return ""
}

// SendApprovalDirective appends an answer for an approval owned by d.Host to
// the checkout's repo session. ID, At and the approver's seat are filled in
// when empty; the directive as sent is returned so the caller can wait for
// its receipt.
func SendApprovalDirective(ctx context.Context, repoRoot string, d ask.RemoteDirective) (ask.RemoteDirective, error) {
	if d.Approval == "" || d.Host == "" || d.Token == "" || d.Verdict == "" {
		return d, errors.New("approval directive needs an approval id, its owner host, the plan token and a verdict")
	}
	if d.ID == "" {
		d.ID = bus.NewPostID()
	}
	if d.At == "" {
		d.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if d.By == "" {
		d.By, _ = SessionParticipant()
		if _, urn := RemoteSenderSignature(); d.ByURN == "" {
			d.ByURN = urn
		}
	}
	sc, err := EnsureRepoSession(ctx, repoRoot)
	if err != nil {
		return d, err
	}
	detail, err := json.Marshal(d)
	if err != nil {
		return d, err
	}
	summary := fmt.Sprintf("%s approval:%s on %s (token %s)", d.Verdict, shortApprovalID(d.Approval), d.Host, d.Token)
	if _, err := sc.client.AppendEvent(ctx, sc.pointer.TaskID, AppendEventReq{Kind: ApprovalDirectiveKind, Summary: summary, Detail: detail}); err != nil {
		return d, err
	}
	return d, nil
}

// FindApprovalReceipt scans the repo session for the owner's receipt for a
// directive. ok is false while the owner has not answered yet.
func FindApprovalReceipt(ctx context.Context, repoRoot, directiveID string) (ask.RemoteReceipt, bool, error) {
	sc, err := EnsureRepoSession(ctx, repoRoot)
	if err != nil {
		return ask.RemoteReceipt{}, false, err
	}
	cursor := ""
	for {
		resp, err := sc.client.GetEvents(ctx, sc.pointer.TaskID, cursor, 100)
		if err != nil {
			return ask.RemoteReceipt{}, false, fmt.Errorf("session feed: %w", err)
		}
		for _, ev := range resp.Events {
			if ev.Kind != ApprovalReceiptKind {
				continue
			}
			var rc ask.RemoteReceipt
			if json.Unmarshal(ev.Detail, &rc) == nil && rc.Directive == directiveID {
				return rc, true, nil
			}
		}
		next := resp.Cursor
		if next == "" && len(resp.Events) > 0 {
			next = resp.Events[len(resp.Events)-1].ID
		}
		if len(resp.Events) < 100 || next == "" || next == cursor {
			return ask.RemoteReceipt{}, false, nil
		}
		cursor = next
	}
}

// approvalLedgerEntry remembers what this host answered for one feed event,
// so a feed redelivery (a second reader's cursor, a crash before the cursor
// moved) neither re-applies nor re-answers it, and a receipt that failed to
// post is re-sent unchanged.
type approvalLedgerEntry struct {
	Receipt ask.RemoteReceipt `json:"receipt"`
	Sent    bool              `json:"sent"`
}

// applyApprovalEvent handles one directive event in a feed drain. It reports
// whether a receipt was posted. An error leaves the feed cursor where it was.
func applyApprovalEvent(ctx context.Context, sc *sessionRepoClient, repoRoot, taskID string, ev Event) (bool, error) {
	var d ask.RemoteDirective
	if json.Unmarshal(ev.Detail, &d) != nil {
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(d.Host), sessionHostName()) {
		return false, nil // another host's queue
	}
	dir, err := weaveQueueDir(repoRoot)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, "approval-events", feedCursorName.ReplaceAllString(taskID+"-"+ev.ID, "-")+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	var entry approvalLedgerEntry
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		// First sight: decide, record the answer, then post it.
		_ = f.Close()
		if reason := bindApprovalAuthor(ctx, sc, taskID, ev, &d); reason != "" {
			entry.Receipt = ask.RemoteReceipt{Directive: d.ID, Approval: d.Approval, Host: d.Host,
				Token: d.Token, Status: ask.RemoteRejected, Reason: reason, TraceID: d.TraceID, At: time.Now().UTC()}
		} else {
			entry.Receipt, _ = ask.ApplyRemote(d, ask.RemotePolicy{Approvers: []string{d.By}})
		}
		entry.Receipt.Host = sessionHostName()
		if err := writeApprovalLedger(path, entry); err != nil {
			return false, err
		}
	case os.IsExist(err):
		b, rerr := os.ReadFile(path)
		if rerr != nil || json.Unmarshal(b, &entry) != nil || entry.Sent || entry.Receipt.Directive == "" {
			// Answered already, or another pass holds the claim.
			return false, nil
		}
	default:
		return false, err
	}
	detail, err := json.Marshal(entry.Receipt)
	if err != nil {
		return false, err
	}
	rc := entry.Receipt
	summary := fmt.Sprintf("%s approval:%s directive %s", rc.Status, shortApprovalID(rc.Approval), rc.Directive)
	if rc.Reason != "" {
		summary += ": " + rc.Reason
	}
	if _, err := sc.client.AppendEvent(ctx, taskID, AppendEventReq{Kind: ApprovalReceiptKind, Summary: summary, Detail: detail}); err != nil {
		return false, fmt.Errorf("approval receipt %s: %w", rc.Directive, err)
	}
	entry.Sent = true
	return true, writeApprovalLedger(path, entry)
}

func writeApprovalLedger(path string, e approvalLedgerEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := weaveWriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func shortApprovalID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
