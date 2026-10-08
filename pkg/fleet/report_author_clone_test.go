package fleet

// Sprint: #379 (checklist 4.19 report/author split, 4.21 clone honesty).
//
// 4.19: an outpost reports what it has (fleet:registry); it never authors the
// catalog. On the yoke side that means the cloud client only READS — every
// request Sync issues is a GET. A token minted for a host must be useless for
// rewriting definitions, and this pins the half of that contract yoke owns.
// 4.21: a clone says so when it cannot inherit context — the empty-note branch
// (a wired cloner with nothing to copy) must still report fresh context.

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The cloud client is read-only by construction: Sync must never issue a
// write (PUT/POST/DELETE) against the catalog API, whatever the noun.
func TestSyncNeverAuthorsCatalogEntries(t *testing.T) {
	var methods []string
	client := CloudClient{
		BaseURL: "https://cloudbox.test",
		Token:   "tok",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			methods = append(methods, r.Method+" "+r.URL.Path)
			var body string
			switch {
			case strings.HasSuffix(r.URL.Path, "/models"):
				body = `{"models":[{"name":"m","kind":"subscription"}]}`
			case strings.HasSuffix(r.URL.Path, "/tools"):
				body = `{"tools":[{"name":"t","content":"name: t\nkind: cli\ncli:\n  binary: t\n  launch:\n    exec: t {prompt}\n"}]}`
			default:
				body = `{"agents":[],"skills":[]}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		})},
	}
	root := t.TempDir()
	for _, noun := range []string{dirModels, dirTools, dirAgents, dirSkills} {
		if _, err := client.Sync(CloudCacheRoot(root), noun); err != nil {
			t.Fatalf("Sync(%s): %v", noun, err)
		}
	}
	if len(methods) == 0 {
		t.Fatal("Sync issued no requests at all")
	}
	for _, m := range methods {
		if !strings.HasPrefix(m, "GET ") {
			t.Errorf("catalog sync issued a non-read %q — the cloud client must never author entries", m)
		}
	}
}

// A wired cloner that has nothing to copy (a tool whose store bashy does
// not relocate) returns an empty note — and the catalog must still report
// fresh context naming the tool, never silence.
func TestCloneContextNamesToolWhenClonerHasNothingToCopy(t *testing.T) {
	cat := New(WithRoot(t.TempDir()), WithContextCloner(
		func(parent, clone Agent) (string, error) { return "", nil }))
	note := cat.cloneContext(Agent{Name: "bruno", Tool: "codex"}, Agent{Name: "bruno2"})
	if !strings.Contains(note, "fresh context") {
		t.Errorf("note = %q, want it to say the clone starts fresh", note)
	}
	if !strings.Contains(note, "codex") {
		t.Errorf("note = %q, want it to name the tool that keeps its own state", note)
	}
}
