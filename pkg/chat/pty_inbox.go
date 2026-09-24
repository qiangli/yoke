package chat

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// ptyInboxBytes bounds the mail typed into a live agent TUI in one go.
//
// A PTY is a keyboard, not a message channel: a steer reaches the TUI as typed
// text, flattened to one line and written in 512-byte bursts. A multi-KB inbox
// block typed into a fresh codex prompt came back as a stream of terminal bells
// (sprint 276 evidence). Mail that does not fit is announced instead, and stays
// unread for the agent to fetch with `bashy inbox`.
const ptyInboxBytes = 1024

// ptyInboxPreviewBytes is how much of an oversized block the notice quotes.
const ptyInboxPreviewBytes = 320

// ptyInbox shapes prepared inbox text for typing into one agent's TUI. It
// remembers the last block it announced, so an unread block is announced once,
// not on every rescan — each announcement is a turn the agent is billed for.
type ptyInbox struct {
	agent string

	mu        sync.Mutex
	announced [sha256.Size]byte
}

func newPTYInbox(agent string) *ptyInbox { return &ptyInbox{agent: agent} }

// deliver types block through say. It returns complete=true when the whole
// block went in, so the caller may acknowledge it. An oversized block is
// replaced by a notice and complete is false: the mail stays unread. An
// oversized block that was already announced is skipped (nothing is said).
func (b *ptyInbox) deliver(block string, say func(string) error) (complete bool, err error) {
	block = strings.TrimSpace(block)
	if block == "" {
		return false, nil
	}
	if len(block) <= ptyInboxBytes {
		if err := say(block); err != nil {
			return false, err
		}
		return true, nil
	}
	sum := sha256.Sum256([]byte(block))
	b.mu.Lock()
	seen := sum == b.announced
	b.mu.Unlock()
	if seen {
		return false, nil
	}
	if err := say(ptyInboxNotice(b.agent, block)); err != nil {
		return false, err
	}
	b.mu.Lock()
	b.announced = sum
	b.mu.Unlock()
	return false, nil
}

// ptyInboxNotice announces an oversized inbox block with a short preview and
// the command that reads it in full.
func ptyInboxNotice(agent, block string) string {
	preview := strings.Join(strings.Fields(block), " ")
	if len(preview) > ptyInboxPreviewBytes {
		cut := ptyInboxPreviewBytes
		for cut > 0 && !utf8.RuneStart(preview[cut]) {
			cut--
		}
		preview = preview[:cut] + " …"
	}
	return fmt.Sprintf("[Bashy inbox] %d bytes of unread mail for %s — too long to type into this session. "+
		"Preview: %s — read it all with: bashy inbox --as %s", len(block), agent, preview, strconv.Quote(agent))
}
