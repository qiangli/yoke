package feed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type memSink struct {
	name   string
	store  map[string][]byte
	putErr error
}

func (m *memSink) Name() string { return m.name }
func (m *memSink) Kind() string { return "mem" }
func (m *memSink) Put(_ context.Context, relPath string, data []byte) (string, error) {
	if m.putErr != nil {
		return "", m.putErr
	}
	if m.store == nil {
		m.store = map[string][]byte{}
	}
	m.store[relPath] = append([]byte(nil), data...)
	return "mem://" + m.name + "/" + relPath, nil
}
func (m *memSink) Get(_ context.Context, ref string) ([]byte, error) {
	data, ok := m.store[ref]
	if !ok {
		return nil, errors.New("mem: not found: " + ref)
	}
	return data, nil
}

func TestPutAllPartialFailure(t *testing.T) {
	good := &memSink{name: "good"}
	bad := &memSink{name: "bad", putErr: errors.New("boom")}
	results := PutAll(context.Background(), []Sink{good, bad}, "p/1.json", []byte(`{"n":1}`))
	if len(results) != 2 {
		t.Fatalf("PutAll returned %d results, want 2", len(results))
	}
	if results[0].Name != "good" || results[1].Name != "bad" {
		t.Fatalf("PutAll order = %q,%q, want good,bad", results[0].Name, results[1].Name)
	}
	if results[0].Err != nil {
		t.Fatalf("good sink failed: %v", results[0].Err)
	}
	if results[0].URL == "" {
		t.Fatal("good sink returned empty URL")
	}
	if results[1].Err == nil {
		t.Fatal("bad sink returned nil error")
	}
	if got := string(good.store["p/1.json"]); got != `{"n":1}` {
		t.Fatalf("good sink stored %q", got)
	}
}

func TestPutAllEmpty(t *testing.T) {
	if results := PutAll(context.Background(), nil, "p/1.json", []byte("x")); len(results) != 0 {
		t.Fatalf("PutAll(nil) returned %d results, want 0", len(results))
	}
}

func TestSinksJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []SinkConfig{
		{Name: "pages", Kind: "github", Target: "https://github.com/o/r.git", Branch: "main"},
		{Name: "nas", Kind: "rclone", Target: "nas:feed"},
	}
	if err := SaveSinks(dir, want); err != nil {
		t.Fatalf("SaveSinks: %v", err)
	}
	got, err := LoadSinks(dir)
	if err != nil {
		t.Fatalf("LoadSinks: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
	for _, c := range got {
		if _, err := OpenSink(c); err != nil {
			t.Fatalf("OpenSink(%#v): %v", c, err)
		}
	}
}

func TestLoadSinksMissing(t *testing.T) {
	got, err := LoadSinks(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("LoadSinks missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LoadSinks missing dir = %#v, want empty", got)
	}
}

func TestSaveSinksDuplicateName(t *testing.T) {
	dir := t.TempDir()
	dup := []SinkConfig{
		{Name: "a", Kind: "github", Target: "https://github.com/o/r.git"},
		{Name: "a", Kind: "rclone", Target: "nas:feed"},
	}
	if err := SaveSinks(dir, dup); err == nil {
		t.Fatal("SaveSinks duplicate names: nil error")
	}
}

func TestLoadSinksDuplicateName(t *testing.T) {
	dir := t.TempDir()
	raw := `[{"name":"a","kind":"github","target":"https://github.com/o/r.git"},{"name":"a","kind":"rclone","target":"nas:feed"}]`
	if err := os.WriteFile(filepath.Join(dir, "sinks.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSinks(dir); err == nil {
		t.Fatal("LoadSinks duplicate names: nil error")
	}
}

func TestSaveSinksBadKind(t *testing.T) {
	dir := t.TempDir()
	bad := []SinkConfig{{Name: "a", Kind: "bogus", Target: "x"}}
	if err := SaveSinks(dir, bad); err == nil {
		t.Fatal("SaveSinks bad kind: nil error")
	}
}

func TestOpenSinkUnknownKind(t *testing.T) {
	if _, err := OpenSink(SinkConfig{Name: "a", Kind: "bogus", Target: "x"}); err == nil {
		t.Fatal("OpenSink bogus kind: nil error")
	}
	if _, err := OpenSink(SinkConfig{Name: "g", Kind: "github"}); err == nil {
		t.Fatal("OpenSink github without target: nil error")
	}
}

func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"a/b.json", "a/b.json", false},
		{"a/./b.json", "a/b.json", false},
		{"", "", true},
		{"/abs.json", "", true},
		{"../evil", "", true},
		{"a/../../evil", "", true},
		{".", "", true},
	}
	for _, c := range cases {
		got, err := validateRelPath(c.in)
		if c.wantErr && err == nil {
			t.Errorf("validateRelPath(%q): nil error", c.in)
		}
		if !c.wantErr && (err != nil || got != c.want) {
			t.Errorf("validateRelPath(%q) = %q,%v; want %q,nil", c.in, got, err, c.want)
		}
	}
}

func TestSinkPublicURLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := []SinkConfig{
		{Name: "a", Kind: "rclone", Target: "r:p", PublicURL: "https://example.test/feed"},
		{Name: "b", Kind: "github", Target: "t"},
	}
	if err := SaveSinks(dir, in); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "sinks.json"))
	if strings.Count(string(raw), "public_url") != 1 {
		t.Fatalf("public_url must be written only when set: %s", raw)
	}
	out, err := LoadSinks(dir)
	if err != nil || len(out) != 2 || out[0].PublicURL != in[0].PublicURL || out[1].PublicURL != "" {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
	bad := []SinkConfig{{Name: "a", Kind: "rclone", Target: "r:p", PublicURL: "ftp://x.test"}}
	if err := SaveSinks(dir, bad); err == nil {
		t.Fatal("SaveSinks accepted a non-http public URL")
	}
}

func TestNormalizePublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                         "",
		"https://x.test":           "https://x.test",
		"https://x.test/feed/":     "https://x.test/feed",
		"http://x.test:8080/a/b//": "http://x.test:8080/a/b",
	} {
		if got, err := normalizePublicURL(in); err != nil || got != want {
			t.Errorf("normalizePublicURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"x.test/feed", "ftp://x.test", "https://", "https:///p", "file:///tmp/x", "https://x.test/?q=1", "https://x.test/#f"} {
		if _, err := normalizePublicURL(in); err == nil {
			t.Errorf("normalizePublicURL(%q) succeeded", in)
		}
	}
}
