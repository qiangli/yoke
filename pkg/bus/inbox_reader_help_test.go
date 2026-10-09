// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package bus

import (
	"strings"
	"testing"
)

// firstParagraph returns the text up to the first blank line.
func firstParagraph(long string) string {
	if i := strings.Index(long, "\n\n"); i >= 0 {
		return long[:i]
	}
	return long
}

// The board-reading front doors must name `bashy inbox` in the FIRST paragraph
// of their Long help, not buried near the examples — so an agent reading mb /
// bus / ping help learns the cursor-safe inbox view up front.
func TestReaderHelpNamesInboxFirstParagraph(t *testing.T) {
	cmds := map[string]string{
		"mb":   NewMessageBoardCmd().Long,
		"bus":  NewBusCmd().Long,
		"ping": NewPingCmd().Long,
	}
	for name, long := range cmds {
		if long == "" {
			t.Errorf("%s: Long help is empty", name)
			continue
		}
		head := firstParagraph(long)
		if !strings.Contains(head, InboxReaderLine) {
			t.Errorf("%s: first paragraph of Long does not contain the inbox reader line.\nfirst paragraph:\n%s", name, head)
		}
	}
}
