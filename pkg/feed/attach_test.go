package feed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAttachFile creates dir/name with content and returns its path.
func writeAttachFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func signedPostWithAttachments(t *testing.T, atts []Attachment) *Post {
	t.Helper()
	priv, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Post{ID: "att1", Author: "a", Created: "2026-03-04T05:06:07Z",
		Headline: "H", Dek: "d", Labels: []string{"fed"}, Body: "the body",
		Attachments: atts}
	if err := SignPost(priv, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- signing ----

func TestSigningPayloadForPostBackwardCompatible(t *testing.T) {
	cases := []struct {
		body, head, dek, reply string
		labels                 []string
	}{
		{"b", "h", "d", "r", []string{"x", "y"}},
		{"", "", "", "", nil},
		{"body with\nnewlines", "héadline", "", "re:1", []string{}},
	}
	for _, c := range cases {
		old := SigningPayload(shaHex(c.body), c.head, c.dek, c.labels, c.reply)
		for _, empty := range [][]Attachment{nil, {}} {
			if got := SigningPayloadForPost(shaHex(c.body), c.head, c.dek, c.labels, c.reply, empty); !bytes.Equal(got, old) {
				t.Fatalf("empty attachments changed the payload for %+v", c)
			}
		}
	}
}

func TestAttachSignVerifyRoundTrip(t *testing.T) {
	atts := []Attachment{
		{Path: "photo.png", SHA256: shaHex("png-bytes")},
		{Path: "talk.mp3", SHA256: shaHex("mp3-bytes")},
	}
	p := signedPostWithAttachments(t, atts)
	if err := VerifyPost(p); err != nil {
		t.Fatalf("fresh post with attachments: %v", err)
	}
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParsePost(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Attachments) != 2 || q.Attachments[0] != atts[0] || q.Attachments[1] != atts[1] {
		t.Fatalf("round-tripped attachments = %+v", q.Attachments)
	}
	if err := VerifyPost(q); err != nil {
		t.Fatalf("round-tripped post: %v", err)
	}

	mutations := map[string]func(*Post){
		"sha tampered":  func(x *Post) { x.Attachments[0].SHA256 = shaHex("other") },
		"path tampered": func(x *Post) { x.Attachments[1].Path = "evil.mp3" },
		"entry dropped": func(x *Post) { x.Attachments = x.Attachments[:1] },
		"entry added":   func(x *Post) { x.Attachments = append(x.Attachments, Attachment{Path: "x", SHA256: shaHex("x")}) },
		"order swapped": func(x *Post) { x.Attachments[0], x.Attachments[1] = x.Attachments[1], x.Attachments[0] },
		"list stripped": func(x *Post) { x.Attachments = nil },
	}
	for name, mutate := range mutations {
		c := *q
		c.Attachments = append([]Attachment(nil), q.Attachments...)
		mutate(&c)
		if err := VerifyPost(&c); !errors.Is(err, ErrDrifted) {
			t.Errorf("%s: want drifted, got %v", name, err)
		}
	}
}

func TestAttachPayloadGolden(t *testing.T) {
	atts := []Attachment{
		{Path: "a.png", SHA256: "aa"},
		{Path: "b.mp3", SHA256: "bb"},
	}
	got := SigningPayloadForPost("BODY", "H", "D", []string{"l"}, "R", atts)
	t.Logf("payload hex: %x", got)
	// Hand-check the layout: v1 tag line, length-prefixed body/head/dek,
	// label count + label, reply_to, then attachment count (2) and each
	// entry's length-prefixed path and sha256.
	want := "64686e742d666565642d7369672d76310a" + // "dhnt-feed-sig-v1\n"
		"00000004424f4459" + // "BODY"
		"0000000148" + // "H"
		"0000000144" + // "D"
		"00000001" + "000000016c" + // 1 label "l"
		"0000000152" + // "R"
		"00000002" + // 2 attachments
		"00000005612e706e67" + "000000026161" + // "a.png", "aa"
		"00000005622e6d7033" + "000000026262" // "b.mp3", "bb"
	if x := hex.EncodeToString(got); x != want {
		t.Fatalf("payload hex mismatch:\n got %s\nwant %s", x, want)
	}
}

// ---- collection ----

func TestCollectAttachmentsBasics(t *testing.T) {
	dir := t.TempDir()
	a := writeAttachFile(t, filepath.Join(dir, "sub"), "photo.png", "png-bytes")
	b := writeAttachFile(t, dir, "talk.mp3", "mp3-bytes")
	entries, uploads, err := collectAttachments([]string{a, b}, "posts/2026/id.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "id/photo.png" || entries[0].SHA256 != shaHex("png-bytes") ||
		entries[1].Path != "id/talk.mp3" || entries[1].SHA256 != shaHex("mp3-bytes") {
		t.Fatalf("entries = %+v", entries)
	}
	if len(uploads) != 2 || uploads[0].rel != "posts/2026/id/photo.png" || string(uploads[0].data) != "png-bytes" ||
		uploads[1].rel != "posts/2026/id/talk.mp3" {
		t.Fatalf("uploads = %+v", uploads)
	}
	if entries, uploads, err := collectAttachments(nil, "posts/2026/id.md", 0); err != nil || len(entries) != 0 || len(uploads) != 0 {
		t.Fatalf("no attachments = %v, %v, %v", entries, uploads, err)
	}
}

func TestCollectAttachmentsRejects(t *testing.T) {
	dir := t.TempDir()
	a := writeAttachFile(t, filepath.Join(dir, "one"), "dup.png", "a")
	_ = writeAttachFile(t, filepath.Join(dir, "two"), "dup.png", "b")
	big := writeAttachFile(t, dir, "big.bin", strings.Repeat("x", 100))

	if _, _, err := collectAttachments([]string{a, filepath.Join(dir, "two", "dup.png")}, "posts/2026/id.md", 0); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate basenames: %v", err)
	}
	if _, _, err := collectAttachments([]string{big}, "posts/2026/id.md", 10); err == nil ||
		!strings.Contains(err.Error(), "over the 10-byte") {
		t.Fatalf("cap: %v", err)
	}
	if _, _, err := collectAttachments([]string{filepath.Join(dir, "missing.png")}, "posts/2026/id.md", 0); err == nil {
		t.Fatal("missing file accepted")
	}
	// an attachment that would overwrite the post itself is rejected
	self := writeAttachFile(t, dir, "id.md", "x")
	if _, _, err := collectAttachments([]string{self}, "posts/2026/id.md", 0); err == nil {
		t.Fatal("self-overwriting attachment accepted")
	}
}

// Two posts sharing a year directory must not clobber each other's
// same-named attachment: each goes under its own stem directory and both
// fetch verified.
func TestCollectAttachmentsSameNameDifferentPosts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f1 := writeAttachFile(t, filepath.Join(dir, "one"), "pic.bin", "first-bytes")
	f2 := writeAttachFile(t, filepath.Join(dir, "two"), "pic.bin", "second-bytes")

	e1, u1, err := collectAttachments([]string{f1}, "posts/2026/aaa.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	e2, u2, err := collectAttachments([]string{f2}, "posts/2026/bbb.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if e1[0].Path != "aaa/pic.bin" || u1[0].rel != "posts/2026/aaa/pic.bin" {
		t.Fatalf("post aaa = %+v %+v", e1, u1)
	}
	if e2[0].Path != "bbb/pic.bin" || u2[0].rel != "posts/2026/bbb/pic.bin" {
		t.Fatalf("post bbb = %+v %+v", e2, u2)
	}
	if u1[0].rel == u2[0].rel {
		t.Fatalf("both posts share one attachment path: %q", u1[0].rel)
	}

	src := &spySource{data: map[string][]byte{
		u1[0].rel: u1[0].data,
		u2[0].rel: u2[0].data,
	}}
	p1 := &Post{ID: "aaa", Attachments: e1}
	p2 := &Post{ID: "bbb", Attachments: e2}
	if d := VerifyPostAttachments(ctx, src, "posts/2026/aaa.md", p1); d != "" {
		t.Fatalf("post aaa: %q", d)
	}
	if d := VerifyPostAttachments(ctx, src, "posts/2026/bbb.md", p2); d != "" {
		t.Fatalf("post bbb: %q", d)
	}
}

func TestValidateAttachmentName(t *testing.T) {
	for _, good := range []string{"a.png", "x.tar.gz", "a b.mp3", ".hidden", "UPPER.JPG"} {
		if err := validateAttachmentName(good); err != nil {
			t.Errorf("validateAttachmentName(%q) = %v", good, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "/abs", "a\x00b"} {
		if err := validateAttachmentName(bad); err == nil {
			t.Errorf("validateAttachmentName(%q) accepted", bad)
		}
	}
	if base, err := attachmentBaseName(filepath.Join("some", "dir", "photo.png")); err != nil || base != "photo.png" {
		t.Fatalf("basename = %q, %v", base, err)
	}
}

func TestAttachmentRel(t *testing.T) {
	rel, err := attachmentRel("posts/2026/id.md", "a.png")
	if err != nil || rel != "posts/2026/id/a.png" {
		t.Fatalf("rel = %q, %v", rel, err)
	}
	// a custom --path uses its own stem too
	rel, err = attachmentRel("posts/x/hello.md", "pic.png")
	if err != nil || rel != "posts/x/hello/pic.png" {
		t.Fatalf("custom path rel = %q, %v", rel, err)
	}
	if _, err := attachmentRel("posts/2026/id.md", "../evil"); err == nil {
		t.Fatal("traversal basename accepted")
	}
	if _, err := attachmentRel("posts/2026/id.md", "id.md"); err == nil {
		t.Fatal("post-overwriting name accepted")
	}
	if _, err := attachmentRel("index.txt", "index.txt"); err == nil {
		t.Fatal("index-overwriting name accepted")
	}
}

func TestResolveAttachmentRef(t *testing.T) {
	ref, err := ResolveAttachmentRef("posts/2026/id.md", "id/photo.png")
	if err != nil || ref != "posts/2026/id/photo.png" {
		t.Fatalf("ref = %q, %v", ref, err)
	}
	// older bare-basename entries keep resolving (forward-compatible)
	ref, err = ResolveAttachmentRef("posts/2026/id.md", "photo.png")
	if err != nil || ref != "posts/2026/photo.png" {
		t.Fatalf("legacy ref = %q, %v", ref, err)
	}
	for _, hostile := range []string{"", "../evil", "../../etc/passwd", "/abs", "a/../../evil"} {
		if _, err := ResolveAttachmentRef("posts/2026/id.md", hostile); err == nil {
			t.Errorf("ResolveAttachmentRef(%q) accepted", hostile)
		}
	}
}

// ---- fetch-side verification ----

type spySource struct {
	data map[string][]byte
	got  []string
}

func (s *spySource) Get(_ context.Context, rel string) ([]byte, error) {
	s.got = append(s.got, rel)
	d, ok := s.data[rel]
	if !ok {
		return nil, errors.New("spy: not found: " + rel)
	}
	return d, nil
}

func TestVerifyPostAttachments(t *testing.T) {
	ctx := context.Background()
	p := &Post{ID: "x", Attachments: []Attachment{
		{Path: "a.png", SHA256: shaHex("AAA")},
		{Path: "b.mp3", SHA256: shaHex("BBB")},
	}}
	src := &spySource{data: map[string][]byte{
		"posts/2026/a.png": []byte("AAA"),
		"posts/2026/b.mp3": []byte("BBB"),
	}}
	if d := VerifyPostAttachments(ctx, src, "posts/2026/id.md", p); d != "" {
		t.Fatalf("matching attachments: %q", d)
	}

	tampered := &spySource{data: map[string][]byte{
		"posts/2026/a.png": []byte("AAAX"),
		"posts/2026/b.mp3": []byte("BBB"),
	}}
	if d := VerifyPostAttachments(ctx, tampered, "posts/2026/id.md", p); !strings.Contains(d, "a.png") || !strings.Contains(d, "mismatch") {
		t.Fatalf("tampered byte: %q", d)
	}

	missing := &spySource{data: map[string][]byte{
		"posts/2026/a.png": []byte("AAA"),
	}}
	if d := VerifyPostAttachments(ctx, missing, "posts/2026/id.md", p); !strings.Contains(d, "b.mp3") || !strings.Contains(d, "missing") {
		t.Fatalf("missing file: %q", d)
	}

	// a hostile listed path is reported, never fetched outside the post dir
	hostile := &Post{ID: "y", Attachments: []Attachment{{Path: "../../etc/passwd", SHA256: shaHex("x")}}}
	spy := &spySource{data: map[string][]byte{"../../etc/passwd": []byte("x")}}
	if d := VerifyPostAttachments(ctx, spy, "posts/2026/id.md", hostile); d == "" {
		t.Fatal("hostile path verified")
	}
	for _, g := range spy.got {
		if !strings.HasPrefix(g, "posts/2026/") {
			t.Fatalf("fetched outside the post dir: %q", g)
		}
	}

	if d := VerifyPostAttachments(ctx, src, "posts/2026/id.md", &Post{ID: "z"}); d != "" {
		t.Fatalf("no attachments: %q", d)
	}
	if d := VerifyPostAttachments(ctx, nil, "posts/2026/id.md", p); d != "" {
		t.Fatalf("nil source: %q", d)
	}
}

func TestIngestAttachmentDrift(t *testing.T) {
	ctx := context.Background()
	p := signedPostWithAttachments(t, []Attachment{{Path: "a.png", SHA256: shaHex("AAA")}})
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	good := &spySource{data: map[string][]byte{"posts/2026/a.png": []byte("AAA")}}
	e, err := ingest(t.TempDir(), "src", "posts/2026/id.md", raw, now, attachmentCheck(ctx, good, "posts/2026/id.md"))
	if err != nil || e == nil || e.Status != StatusVerified {
		t.Fatalf("matching attachment = %+v, %v", e, err)
	}

	bad := &spySource{data: map[string][]byte{"posts/2026/a.png": []byte("AAAX")}}
	e, err = ingest(t.TempDir(), "src", "posts/2026/id.md", raw, now, attachmentCheck(ctx, bad, "posts/2026/id.md"))
	if err != nil || e == nil || e.Status != StatusDrifted || !strings.Contains(e.Detail, "a.png") {
		t.Fatalf("tampered attachment = %+v, %v", e, err)
	}

	absent := &spySource{data: map[string][]byte{}}
	e, err = ingest(t.TempDir(), "src", "posts/2026/id.md", raw, now, attachmentCheck(ctx, absent, "posts/2026/id.md"))
	if err != nil || e == nil || e.Status != StatusDrifted || !strings.Contains(e.Detail, "missing") {
		t.Fatalf("missing attachment = %+v, %v", e, err)
	}

	// unsigned posts stay unsigned: without a signature the list is untrusted
	un := &Post{ID: "u", Body: "x", Attachments: []Attachment{{Path: "a.png", SHA256: shaHex("ZZZ")}}}
	ub, _ := un.Marshal()
	e, err = ingest(t.TempDir(), "src", "posts/2026/u.md", ub, now, attachmentCheck(ctx, absent, "posts/2026/u.md"))
	if err != nil || e == nil || e.Status != StatusUnsigned {
		t.Fatalf("unsigned = %+v, %v", e, err)
	}
}

// ---- sink coverage: git and rclone kinds ----

// firstPostMarkdown finds the single published post markdown under root,
// ignoring attachments stored beside it.
func firstPostMarkdown(t *testing.T, root string) string {
	t.Helper()
	var found string
	filepath.WalkDir(filepath.Join(root, "posts"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no post markdown under %s", root)
	}
	return found
}

type kindSink struct {
	Sink
	kind string
}

func (k kindSink) Kind() string { return k.kind }

func TestAttachPutReachesGitAndRcloneSinks(t *testing.T) {
	ctx := context.Background()
	git := kindSink{Sink: &memSink{name: "g"}, kind: "github"}
	rclone := kindSink{Sink: &dirSink{name: "r", root: t.TempDir()}, kind: "rclone"}
	data := []byte("attachment bytes")
	results := PutAll(ctx, []Sink{git, rclone}, "posts/2026/a.png", data)
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil || r.URL == "" {
			t.Fatalf("result = %+v", r)
		}
	}
	back, err := git.Get(ctx, "posts/2026/a.png")
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("git sink content = %q, %v", back, err)
	}
	back, err = rclone.Get(ctx, "posts/2026/a.png")
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("rclone sink content = %q, %v", back, err)
	}
}

// ---- end to end through the CLI ----

func TestPostAttachEndToEnd(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	aDir, bDir := t.TempDir(), t.TempDir()
	te.mustRun("sink", "add", "rclone", "a", aDir)
	te.mustRun("sink", "add", "rclone", "b", bDir)

	files := t.TempDir()
	f1 := writeAttachFile(t, files, "photo.png", "png-bytes")
	f2 := writeAttachFile(t, files, "talk.mp3", "mp3-bytes")
	out := te.mustRun("post", writeBody(t, "body with attachments"), "--attach", f1, "--attach", f2)
	if strings.Count(out, "ok\t") != 6 { // post + 2 attachments, one ok line per sink each
		t.Fatalf("post output = %q", out)
	}
	for _, root := range []string{aDir, bDir} {
		file := firstPostMarkdown(t, root)
		raw, _ := os.ReadFile(file)
		p, err := ParsePost(raw)
		if err != nil {
			t.Fatal(err)
		}
		stem := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		if len(p.Attachments) != 2 || p.Attachments[0].Path != stem+"/photo.png" ||
			p.Attachments[0].SHA256 != shaHex("png-bytes") || p.Attachments[1].Path != stem+"/talk.mp3" ||
			p.Attachments[1].SHA256 != shaHex("mp3-bytes") {
			t.Fatalf("frontmatter attachments = %+v", p.Attachments)
		}
		if err := VerifyPost(p); err != nil {
			t.Fatalf("signed attachments do not verify: %v", err)
		}
		for _, name := range []string{"photo.png", "talk.mp3"} {
			dir := filepath.Dir(file)
			got, err := os.ReadFile(filepath.Join(dir, stem, name))
			if err != nil {
				t.Fatalf("attachment %s under the post dir: %v", name, err)
			}
			want := map[string]string{"photo.png": "png-bytes", "talk.mp3": "mp3-bytes"}[name]
			if string(got) != want {
				t.Fatalf("attachment %s = %q", name, got)
			}
		}
	}

	te.mustRun("follow", aDir)
	if out := te.mustRun("fetch"); !strings.Contains(out, "\tverified\t") {
		t.Fatalf("fetch = %q", out)
	}

	// tamper one attachment byte: a fresh follower sees drifted
	tainted := firstPostMarkdown(t, aDir)
	raw, _ := os.ReadFile(tainted)
	pp, _ := ParsePost(raw)
	attPath := filepath.Join(filepath.Dir(tainted), pp.Attachments[0].Path)
	ab, _ := os.ReadFile(attPath)
	ab[0] ^= 0xff
	if err := os.WriteFile(attPath, ab, 0o644); err != nil {
		t.Fatal(err)
	}
	te2 := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te2.mustRun("follow", aDir)
	if out := te2.mustRun("fetch"); !strings.Contains(out, "\tdrifted\t") {
		t.Fatalf("tampered-attachment fetch = %q", out)
	}

	// a missing attachment is drifted too
	if err := os.Remove(attPath); err != nil {
		t.Fatal(err)
	}
	te3 := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te3.mustRun("follow", aDir)
	out3, _, err3 := te3.run("fetch")
	if err3 != nil {
		t.Fatal(err3)
	}
	if !strings.Contains(out3, "\tdrifted\t") {
		t.Fatalf("missing-attachment fetch = %q", out3)
	}
}

func TestPostAttachCapEnforced(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te.env.MaxAttachBytes = 32
	te.mustRun("sink", "add", "rclone", "local", t.TempDir())
	big := writeAttachFile(t, t.TempDir(), "big.bin", strings.Repeat("x", 100))
	_, _, err := te.run("post", writeBody(t, "body"), "--attach", big)
	if err == nil || !strings.Contains(err.Error(), "over the 32-byte") {
		t.Fatalf("cap error = %v", err)
	}
}

func TestPostAttachDuplicateRejected(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te.mustRun("sink", "add", "rclone", "local", t.TempDir())
	d1, d2 := t.TempDir(), t.TempDir()
	f1 := writeAttachFile(t, d1, "same.png", "a")
	f2 := writeAttachFile(t, d2, "same.png", "b")
	_, _, err := te.run("post", writeBody(t, "body"), "--attach", f1, "--attach", f2)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}
}
