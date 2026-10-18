package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A command hint asks for unread board posts even when the cursor is already
// at the head. Historical post bodies must not be decoded on that path.
func TestUnseenAtHeadOnPopulatedBoardIsBounded(t *testing.T) {
	boardInTempHome(t)
	const count = 12000
	var board bytes.Buffer
	body := string(bytes.Repeat([]byte("x"), 2048))
	for seq := int64(1); seq <= count; seq++ {
		line, err := json.Marshal(Post{SchemaVersion: BoardSchema, Seq: seq, From: "sender", Body: body})
		if err != nil {
			t.Fatal(err)
		}
		board.Write(line)
		board.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(BoardDir(), "posts.jsonl"), board.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MarkSeen("reader", count); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	directed, other, older, err := Unseen("reader", 0)
	elapsed := time.Since(start)
	t.Logf("populated board: %d posts, %d bytes, Unseen at head %s", count, board.Len(), elapsed)
	if err != nil || len(directed) != 0 || len(other) != 0 || older != 0 {
		t.Fatalf("Unseen = %d directed, %d other, %d older, %v", len(directed), len(other), older, err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Unseen at head took %s; hint must not decode old posts", elapsed)
	}
	// A later arrival must still be visible, and a hint must never advance the
	// reader's cursor.
	f, err := os.OpenFile(filepath.Join(BoardDir(), "posts.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(Post{SchemaVersion: BoardSchema, Seq: count + 1, From: "sender", Body: "new mail"})
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	_, other, _, err = Unseen("reader", 0)
	elapsed = time.Since(start)
	t.Logf("populated board: one new post after %d old posts, Unseen %s", count, elapsed)
	if err != nil || len(other) != 1 || other[0].Body != "new mail" || SeenSeq("reader") != count {
		t.Fatalf("new arrival = %+v, cursor %d, err %v", other, SeenSeq("reader"), err)
	}
	if elapsed > 20*time.Millisecond {
		t.Fatalf("Unseen for one new arrival took %s; hint must not decode old posts", elapsed)
	}
}

func TestUnseenTailProbeKeepsMalformedAndPartialTailSemantics(t *testing.T) {
	boardInTempHome(t)
	path := filepath.Join(BoardDir(), "posts.jsonl")
	first, _ := json.Marshal(Post{SchemaVersion: BoardSchema, Seq: 1, From: "sender", Body: "old"})
	second, _ := json.Marshal(Post{SchemaVersion: BoardSchema, Seq: 2, From: "sender", Body: "new"})
	third, _ := json.Marshal(Post{SchemaVersion: BoardSchema, Seq: 3, From: "sender", Body: "later"})
	partial := len(third) / 2
	data := append(append(append(append([]byte{}, first...), '\n'), []byte("{broken}\n")...), second...)
	data = append(data, '\n')
	data = append(data, third[:partial]...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MarkSeen("reader", 1); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		_, other, older, err := Unseen("reader", 0)
		if err != nil || len(other) != want || older != 0 || SeenSeq("reader") != 1 {
			t.Fatalf("Unseen = %+v, older %d, cursor %d, err %v; want %d posts", other, older, SeenSeq("reader"), err, want)
		}
		if other[0].Seq != 2 || (want == 2 && other[1].Seq != 3) {
			t.Fatalf("Unseen posts = %+v", other)
		}
	}
	check(1)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(third[partial:], '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	check(2)
}

func TestUnseenTailProbeFallsBackForLargePost(t *testing.T) {
	boardInTempHome(t)
	path := filepath.Join(BoardDir(), "posts.jsonl")
	first, _ := json.Marshal(Post{SchemaVersion: BoardSchema, Seq: 1, From: "sender", Body: "old"})
	second, _ := json.Marshal(Post{SchemaVersion: BoardSchema, Seq: 2, From: "sender", Body: string(bytes.Repeat([]byte("x"), 70*1024))})
	data := append(append(append([]byte{}, first...), '\n'), second...)
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MarkSeen("reader", 1); err != nil {
		t.Fatal(err)
	}
	_, other, older, err := Unseen("reader", 0)
	if err != nil || len(other) != 1 || other[0].Seq != 2 || older != 0 || SeenSeq("reader") != 1 {
		t.Fatalf("Unseen = %+v, older %d, cursor %d, err %v", other, older, SeenSeq("reader"), err)
	}
}
