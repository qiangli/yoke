// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package chat

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

// The chat timeline reader names `bashy inbox` in the FIRST paragraph of its
// Long help, the same pointer every other board-reading front door opens with.
func TestChatTimelineHelpNamesInboxFirstParagraph(t *testing.T) {
	long := newChatTimelineCmd().Long
	if long == "" {
		t.Fatal("timeline Long help is empty")
	}
	head := long
	if i := strings.Index(long, "\n\n"); i >= 0 {
		head = long[:i]
	}
	if !strings.Contains(head, bus.InboxReaderLine) {
		t.Errorf("timeline: first paragraph of Long does not contain the inbox reader line.\nfirst paragraph:\n%s", head)
	}
}
