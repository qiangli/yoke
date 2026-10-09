package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// An empty `mb --json` read must print one empty envelope line, so a JSON
// consumer sees a typed "no posts" rather than no output at all.
func TestMessageBoardJSONEmptyEnvelope(t *testing.T) {
	boardInTempHome(t)
	out, errOut, err := runMessageBoard(t, context.Background(), "--as", "reader-json-empty", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if errOut != "" {
		t.Fatalf("empty --json read must stay off stderr, got %q", errOut)
	}
	var env struct {
		SchemaVersion string            `json:"schema_version"`
		Posts         []json.RawMessage `json:"posts"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("empty --json read must emit one JSON object, got %q: %v", out, err)
	}
	if env.SchemaVersion != BoardSchema {
		t.Fatalf("schema_version = %q, want %q", env.SchemaVersion, BoardSchema)
	}
	if env.Posts == nil || len(env.Posts) != 0 {
		t.Fatalf("posts must normalize to [], got %q", out)
	}
	if dec.More() {
		t.Fatalf("empty --json read must emit exactly one line, got %q", out)
	}
}

// A non-empty read is untouched: one Post object per line, no envelope.
func TestMessageBoardJSONNonEmptyUnchanged(t *testing.T) {
	boardInTempHome(t)
	if err := PostMessage(Post{From: "carol", To: "dave", Body: "hello dave"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runMessageBoard(t, context.Background(), "--as", "dave", "--json", "--peek")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `"posts"`) {
		t.Fatalf("non-empty --json read must not gain an envelope, got %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one post line, got %q", out)
	}
	var p Post
	if err := json.Unmarshal([]byte(lines[0]), &p); err != nil {
		t.Fatalf("post line must decode as a Post, got %q: %v", out, err)
	}
	if p.Body != "hello dave" || p.SchemaVersion != BoardSchema {
		t.Fatalf("unexpected post %+v", p)
	}
}
