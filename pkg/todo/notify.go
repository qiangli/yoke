// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

// Cell A of the agent comms matrix (sprint #111, story ddd2176106b6):
// --owner was once stored as a free-text assignee field nobody read. Assigning work told nobody,
// and nothing on the item said whether the name typed into it was even
// reachable — "whois todo:<id>" answered "names nothing on this host"
// because todo is not a principal kind, and there was no way to find out
// short of asking a human to go try.
//
// notifyAssignee closes both directions at once, over the EXISTING bus
// notify front door (`bashy notify`, pkg/bus/notify.go) — the same one-line
// subject channel `bashy inbox` already drains — rather than inventing a
// todo-specific channel or teaching whois a new "todo:" address form. It is
// called only from the CLI layer (cli.go), never from the pure Add/SetStatus
// business functions: those stay hermetic and side-effect free, exactly as
// today's tests assume, and the comms attempt is a property of RUNNING the
// command, not of building an *issue.Issue in memory.
//
// It is deliberately best-effort: the assignee field is documented (see
// docs/orchestration-roles.md upstream) as optional and unvalidated, so a
// todo write must never fail because the fabric could not resolve a name a
// human typed by hand. The caller gets an honest AssignmentNotice back
// instead — reachable or not, and why — which is what actually answers "can
// I reach the assignee through the item" without guessing.
import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/issue"
)

// AssignmentNotice is the outcome of trying to reach an item's assignee
// through the existing comms surface at the moment it was assigned.
type AssignmentNotice struct {
	Assignee string
	Notified bool
	Reason   string // why Notified is false; empty when Notified is true or there is no assignee
}

// notifyAssignee tells an item's assignee it now owns work, addressed to
// it.Assignee over the bus notify front door. A blank assignee is a no-op
// notice, not an error.
func notifyAssignee(kind string, it *issue.Issue) AssignmentNotice {
	assignee := strings.TrimSpace(it.Assignee)
	notice := AssignmentNotice{Assignee: assignee}
	if assignee == "" {
		return notice
	}
	from, err := bus.ResolveAuthoredActor("")
	if err != nil {
		notice.Reason = err.Error()
		return notice
	}
	if err := bus.NotifyEvent(from, assignee, assignmentSubject(kind, it)); err != nil {
		notice.Reason = err.Error()
		return notice
	}
	notice.Notified = true
	return notice
}

// assignmentSubject builds the one-line notify subject, capped to
// bus.MaxNotifySubjectBytes (notify refuses an overlong subject rather than
// truncating it, so this function truncates first) and with newlines folded
// to spaces (notify requires one line).
func assignmentSubject(kind string, it *issue.Issue) string {
	title := strings.NewReplacer("\r", " ", "\n", " ").Replace(it.Title)
	subject := fmt.Sprintf("%s assigned: %s (%s)", kind, title, shortID(it.ID))
	return truncateBytes(subject, bus.MaxNotifySubjectBytes)
}

// truncateBytes cuts s to at most max bytes without splitting a UTF-8
// sequence.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
