package chat

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPTYInboxTypesASmallBlockWhole(t *testing.T) {
	var said []string
	complete, err := newPTYInbox("a").deliver("  mail  ", func(s string) error { said = append(said, s); return nil })
	if err != nil || !complete {
		t.Fatalf("complete=%v err=%v", complete, err)
	}
	if len(said) != 1 || said[0] != "mail" {
		t.Fatalf("said = %q", said)
	}
}

// A multi-KB block typed into codex came back as terminal bells. It is announced
// instead, left unread, and announced ONCE — the relay rescans every 30s, and
// every announcement is a billed turn.
func TestPTYInboxOverBudgetAnnouncesOnceAndLeavesTheMailUnread(t *testing.T) {
	b := newPTYInbox("codex-x")
	block := "[Bashy unified inbox — read before the instruction below]\n" + strings.Repeat("mb:1 from lead: status please\n", 200)
	var said []string
	say := func(s string) error { said = append(said, s); return nil }
	for i := 0; i < 3; i++ {
		complete, err := b.deliver(block, say)
		if err != nil || complete {
			t.Fatalf("round %d: complete=%v err=%v; an oversized block must not be acknowledged", i, complete, err)
		}
	}
	if len(said) != 1 {
		t.Fatalf("announced %d times, want once", len(said))
	}
	n := said[0]
	if len(n) > ptyInboxBytes || !strings.Contains(n, `bashy inbox --as "codex-x"`) || strings.Contains(n, "\n") {
		t.Fatalf("notice = %q (%d bytes)", n, len(n))
	}
	// New mail changes the block: announced again.
	if _, err := b.deliver(block+"mb:2 from lead: ping\n", say); err != nil || len(said) != 2 {
		t.Fatalf("a changed block was not announced: said=%d err=%v", len(said), err)
	}
}

// A refused write is retried: the notice is not remembered as announced.
func TestPTYInboxRetriesARefusedAnnouncement(t *testing.T) {
	b := newPTYInbox("a")
	block := strings.Repeat("x", ptyInboxBytes+1)
	if _, err := b.deliver(block, func(string) error { return errors.New("socket gone") }); err == nil {
		t.Fatal("expected the refusal")
	}
	calls := 0
	if _, err := b.deliver(block, func(string) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("retry calls=%d err=%v", calls, err)
	}
}

func TestPTYInboxNoticeCutsOnARuneBoundary(t *testing.T) {
	n := ptyInboxNotice("a", strings.Repeat("é", ptyInboxBytes))
	if !strings.Contains(n, " …") || !utf8.ValidString(n) {
		t.Fatalf("notice = %q", n)
	}
}
