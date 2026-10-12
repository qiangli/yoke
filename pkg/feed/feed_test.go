package feed

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Post sign / verify ----

func newSignedPost(t *testing.T) (*Post, string) {
	t.Helper()
	dir := t.TempDir()
	priv, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &Post{ID: "p1", Author: "a", Created: "2026-01-02T03:04:05Z",
		Headline: "Rates hold", Dek: "Nothing moved.", Labels: []string{"fed"}, Body: "the body"}
	if err := SignPost(priv, p); err != nil {
		t.Fatal(err)
	}
	return p, dir
}

func TestVerifyPostKinds(t *testing.T) {
	p, _ := newSignedPost(t)
	if err := VerifyPost(p); err != nil {
		t.Fatalf("fresh post: %v", err)
	}

	// survives a Marshal/Parse round trip
	b, _ := p.Marshal()
	q, err := ParsePost(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPost(q); err != nil {
		t.Fatalf("round-tripped post: %v", err)
	}

	tests := map[string]func(*Post){
		"body":     func(x *Post) { x.Body = "tampered" },
		"headline": func(x *Post) { x.Headline = "Rates cut" },
		"dek":      func(x *Post) { x.Dek = "Everything moved." },
		"labels":   func(x *Post) { x.Labels = []string{"other"} },
		"reply_to": func(x *Post) { x.ReplyTo = "other" },
		"sig":      func(x *Post) { x.Sig = "AAAA" },
		"nosig":    func(x *Post) { x.Sig = "" },
	}
	for name, mutate := range tests {
		c := *q
		mutate(&c)
		err := VerifyPost(&c)
		if !errors.Is(err, ErrDrifted) {
			t.Errorf("%s: want drifted, got %v", name, err)
		}
	}

	u := *q
	u.Pubkey, u.Sig = "", ""
	if err := VerifyPost(&u); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("want unsigned, got %v", err)
	}
	if st, _ := verifyStatus(&u); st != StatusUnsigned {
		t.Fatalf("status = %s", st)
	}
}

func TestSignPostRejectsBadInput(t *testing.T) {
	if err := SignPost(nil, &Post{}); err == nil {
		t.Fatal("empty key accepted")
	}
	if err := VerifyPost(nil); err == nil {
		t.Fatal("nil post accepted")
	}
}

// ---- cursor ----

func TestCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if n, err := ReadCursor(dir, "alice"); err != nil || n != 0 {
		t.Fatalf("missing cursor = %d,%v", n, err)
	}
	if err := WriteCursor(dir, "alice", 7); err != nil {
		t.Fatal(err)
	}
	if n, _ := ReadCursor(dir, "alice"); n != 7 {
		t.Fatalf("cursor = %d", n)
	}
	if n, _ := ReadCursor(dir, "bob"); n != 0 {
		t.Fatalf("cursors are per-subscriber, bob = %d", n)
	}
	// hostile names stay inside cursors/
	if err := WriteCursor(dir, "../../evil", 3); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "cursors"))
	if len(entries) != 2 {
		t.Fatalf("cursor files = %d", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil")); err == nil {
		t.Fatal("cursor escaped its directory")
	}
	// corrupt rewinds to 0
	os.WriteFile(filepath.Join(dir, "cursors", "alice"), []byte("junk"), 0o600)
	if n, _ := ReadCursor(dir, "alice"); n != 0 {
		t.Fatalf("corrupt cursor = %d", n)
	}
}

// ---- spool ----

func spoolPost(t *testing.T, id, body string, sign bool) []byte {
	t.Helper()
	p := &Post{ID: id, Author: "a", Headline: "H " + id, Dek: "d", Body: body}
	if sign {
		priv, _ := LoadOrCreateKey(t.TempDir())
		if err := SignPost(priv, p); err != nil {
			t.Fatal(err)
		}
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIngestDedupeAndDrift(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	raw := spoolPost(t, "id1", "one", true)

	e, err := Ingest(dir, "src", "posts/a.md", raw, now)
	if err != nil || e == nil || e.Seq != 1 || e.Status != StatusVerified {
		t.Fatalf("first ingest = %+v, %v", e, err)
	}
	if e, err := Ingest(dir, "src", "posts/a.md", raw, now); err != nil || e != nil {
		t.Fatalf("same id+hash must dedupe, got %+v, %v", e, err)
	}

	changed := []byte(strings.Replace(string(raw), "one", "two", 1))
	e, err = Ingest(dir, "src", "posts/a.md", changed, now)
	if err != nil || e == nil || e.Seq != 2 || e.Status != StatusDrifted {
		t.Fatalf("changed body = %+v, %v", e, err)
	}

	un, err := Ingest(dir, "src", "posts/b.md", spoolPost(t, "id2", "x", false), now)
	if err != nil || un.Seq != 3 || un.Status != StatusUnsigned {
		t.Fatalf("unsigned = %+v, %v", un, err)
	}

	all, _ := ReadSpool(dir)
	if len(all) != 3 || all[0].Seq != 1 || all[2].Seq != 3 {
		t.Fatalf("spool = %+v", all)
	}
	if _, err := Ingest(dir, "src", "x.md", []byte("no frontmatter"), now); err == nil {
		t.Fatal("garbage ingested")
	}
}

// ---- sources / index ----

func TestParseIndexDropsHostileEntries(t *testing.T) {
	rels, skipped := ParseIndex([]byte("posts/a.md\n\n../../etc/passwd\n/abs.md\nposts/a.md\nindex.txt\nposts/b.md"))
	if strings.Join(rels, ",") != "posts/a.md,posts/b.md" {
		t.Fatalf("rels = %v", rels)
	}
	if len(skipped) != 3 {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestNormalizeFollow(t *testing.T) {
	if got, _ := NormalizeFollow("https://x.example/feed/"); got != "https://x.example/feed" {
		t.Fatalf("got %q", got)
	}
	if got, _ := NormalizeFollow("remote:path/dir/"); got != "remote:path/dir" {
		t.Fatalf("got %q", got)
	}
	d := t.TempDir()
	if got, _ := NormalizeFollow(d); got != d {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"", "nope"} {
		if _, err := NormalizeFollow(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestHTTPSourceAndDirSource(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "posts"), 0o755)
	os.WriteFile(filepath.Join(root, "posts", "a.md"), []byte("hi"), 0o644)
	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	defer srv.Close()

	for name, ref := range map[string]string{"http": srv.URL + "/", "dir": root} {
		ref, _ = NormalizeFollow(ref)
		src, err := OpenSource(ref, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, err := src.Get(context.Background(), "posts/a.md")
		if err != nil || string(b) != "hi" {
			t.Fatalf("%s: %q, %v", name, b, err)
		}
		if _, err := src.Get(context.Background(), "posts/missing.md"); err == nil {
			t.Fatalf("%s: missing file read", name)
		}
		if _, err := src.Get(context.Background(), "../escape"); err == nil {
			t.Fatalf("%s: traversal allowed", name)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(fs.ErrNotExist) || !isNotFound(errors.New("feed: rclone cat x: object not found")) {
		t.Fatal("not-found not recognized")
	}
	if isNotFound(errors.New("connection refused")) {
		t.Fatal("transient error treated as not found")
	}
}
