package meet

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMeetUnreadSplitsDirectedAndCapsOnlyOther(t *testing.T) {
	st := newTestSession(t)
	for _, e := range []Event{
		{Speaker: "qiangli", Kind: "human", Text: "one"},
		{Speaker: "codex", Kind: "turn", Text: "mine one"},
		{Speaker: "opencode", Kind: "turn", Text: "two"},
		{Speaker: "claude", Kind: "turn", Text: "three"},
		{Speaker: "qiangli", Kind: "human", To: "codex", Text: "@codex: please check this"},
		{Speaker: "codex", Kind: "turn", Text: "mine two"},
		{Speaker: "qiangli", Kind: "human", Text: "four"},
		{Speaker: "opencode", Kind: "turn", Text: "five"},
	} {
		if err := AppendEvent(st.ID, e); err != nil {
			t.Fatal(err)
		}
	}
	directed, other, older, err := Unread(st.ID, "codex", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 || directed[0].Text != "@codex: please check this" {
		t.Fatalf("directed = %+v", directed)
	}
	if len(other) != 2 || other[0].Text != "four" || other[1].Text != "five" {
		t.Fatalf("other = %+v", other)
	}
	if older != 3 {
		t.Fatalf("older = %d, want 3", older)
	}
}

func TestBoardPostRejectsOversizedCoordinationBeforeTranscriptAppend(t *testing.T) {
	st := boardRoom(t)
	if _, err := PostAs(st.ID, "codex", "", strings.Repeat("x", 1025)); err == nil ||
		!strings.Contains(err.Error(), "1025 UTF-8 bytes") || !strings.Contains(err.Error(), "max 1024") {
		t.Fatalf("oversized meet tell error = %v", err)
	}
	if events, err := readTranscript(st.ID); err != nil || len(events) != 0 {
		t.Fatalf("rejected meet tell mutated transcript: events=%d err=%v", len(events), err)
	}
	if _, err := PostAs(st.ID, "codex", "", strings.Repeat("é", 512)); err != nil {
		t.Fatalf("1024-byte multibyte meet tell rejected: %v", err)
	}
	if events, err := readTranscript(st.ID); err != nil || len(events) != 1 {
		t.Fatalf("accepted meet tell transcript: events=%d err=%v", len(events), err)
	} else if events[0].Text != strings.Repeat("é", 512) || len(events[0].Text) != 1024 {
		t.Fatalf("accepted multibyte meet tell changed: bytes=%d", len(events[0].Text))
	}
}

func TestBoardPostRejectsAuthenticatedCrossIdentityBeforeTranscriptAppend(t *testing.T) {
	st := boardRoom(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/codex")
	if _, err := PostAs(st.ID, "opencode", "", "REJECTED-MEET-BODY"); err == nil ||
		!strings.Contains(err.Error(), "cannot author as") {
		t.Fatalf("cross-identity meet tell error = %v", err)
	}
	if events, err := readTranscript(st.ID); err != nil || len(events) != 0 {
		t.Fatalf("rejected meet tell mutated transcript: events=%d err=%v", len(events), err)
	}
}

func TestManualTellRejectsOversizedOrdinaryMeetingBeforeTranscriptAppend(t *testing.T) {
	st := newTestSession(t)
	cmd := newTellCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{st.ID, strings.Repeat("x", 1025)})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "1025 UTF-8 bytes") {
		t.Fatalf("oversized ordinary meet tell error = %v", err)
	}
	if events, rerr := readTranscript(st.ID); rerr != nil || len(events) != 0 {
		t.Fatalf("rejected ordinary meet tell mutated transcript: events=%d err=%v", len(events), rerr)
	}
}

func TestMeetUnreadSequenceIsParsedEventIndex(t *testing.T) {
	st := newTestSession(t)
	dir, err := storeDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"speaker":"qiangli","kind":"human","text":"first"}`,
		`{not json}`,
		``,
		`{"speaker":"qiangli","kind":"human","text":"second"}`,
		``,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := markSeenSeq(st.ID, "codex", 1); err != nil {
		t.Fatal(err)
	}
	_, other, older, err := Unread(st.ID, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	if older != 0 || len(other) != 1 || other[0].Text != "second" {
		t.Fatalf("Unread = other %+v older %d", other, older)
	}
	if err := MarkSeen(st.ID, "codex"); err != nil {
		t.Fatal(err)
	}
	if got := SeenSeq(st.ID, "codex"); got != 2 {
		t.Fatalf("SeenSeq = %d, want parsed head 2", got)
	}
}

func TestMeetUnreadReturnsHardErrorForOversizeLine(t *testing.T) {
	st := newTestSession(t)
	dir, err := storeDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(strings.Repeat("x", maxTranscriptLine+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Unread(st.ID, "codex", DefaultRoomLimit); err == nil {
		t.Fatal("Unread accepted an oversize transcript line")
	}
}

func TestMeetWaitForRoomTimeoutIsEmptySuccess(t *testing.T) {
	st := newTestSession(t)
	start := time.Now()
	if err := waitForRoom(context.Background(), st.ID, "codex", DefaultRoomLimit, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waitForRoom did not respect its bound")
	}
}

func TestMeetWaitForRoomWakesOnUnreadEvent(t *testing.T) {
	st := newTestSession(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = AppendEvent(st.ID, Event{Speaker: "qiangli", Kind: "human", Text: "wake"})
	}()
	if err := waitForRoom(context.Background(), st.ID, "codex", DefaultRoomLimit, time.Second); err != nil {
		t.Fatal(err)
	}
	_, other, _, err := Unread(st.ID, "codex", DefaultRoomLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Text != "wake" {
		t.Fatalf("Unread after wait = %+v", other)
	}
}

func TestMeetAppendEventUsesDistinctAppendLock(t *testing.T) {
	st := newTestSession(t)
	run, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Release()
	if err := AppendEvent(st.ID, Event{Speaker: "qiangli", Kind: "human", Text: "while running"}); err != nil {
		t.Fatalf("AppendEvent must not contend on run.lock: %v", err)
	}
	dir, _ := storeDir(st.ID)
	if _, err := os.Stat(filepath.Join(dir, "append.lock")); err != nil {
		t.Fatalf("append.lock not created: %v", err)
	}
}
