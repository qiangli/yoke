package sdlc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeLoomState struct {
	issues    []LoomIssue
	labels    map[string]int64 // name -> id
	labelAdds map[int][]int64  // issue number -> label ids added
}

// fakeLoom routes the Gitea endpoints the mirror uses: issues list/create, labels
// list/create, and add-labels-to-issue. Stateful.
func fakeLoom(t *testing.T, seed []LoomIssue) (*httptest.Server, *fakeLoomState) {
	t.Helper()
	var mu sync.Mutex
	st := &fakeLoomState{issues: append([]LoomIssue{}, seed...), labels: map[string]int64{}, labelAdds: map[int][]int64{}}
	nextIssue, nextLabel := 100, int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/labels") && r.Method == http.MethodGet:
			out := []map[string]any{}
			for n, id := range st.labels {
				out = append(out, map[string]any{"id": id, "name": n})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(p, "/labels") && r.Method == http.MethodPost && strings.Contains(p, "/issues/"):
			// add labels to issue: /repos/x/issues/N/labels
			var body struct{ Labels []int64 }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			var num int
			fmt.Sscanf(p, "/api/v1/repos/loom/site/issues/%d/labels", &num)
			st.labelAdds[num] = append(st.labelAdds[num], body.Labels...)
			_ = json.NewEncoder(w).Encode([]any{})
		case strings.HasSuffix(p, "/labels") && r.Method == http.MethodPost:
			// create repo label
			var body struct{ Name, Color string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			nextLabel++
			st.labels[strings.ToLower(body.Name)] = nextLabel
			_ = json.NewEncoder(w).Encode(map[string]any{"id": nextLabel, "name": body.Name})
		case strings.HasSuffix(p, "/issues") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(st.issues)
		case strings.HasSuffix(p, "/issues") && r.Method == http.MethodPost:
			var body struct{ Title, Body string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			nextIssue++
			li := LoomIssue{Number: nextIssue, Title: body.Title, Body: body.Body, State: "open"}
			st.issues = append(st.issues, li)
			_ = json.NewEncoder(w).Encode(li)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func ghPR() *struct {
	URL string `json:"url"`
} {
	return &struct {
		URL string `json:"url"`
	}{URL: "x"}
}

func TestMirrorGitHubIssuesToLoom(t *testing.T) {
	fakeGitHub(t, []GitHubIssue{
		{Number: 1, Title: "already mirrored", State: "open", HTML: "https://github.com/acme/site/issues/1"},
		{Number: 2, Title: "new request", Body: "please fix", State: "open", HTML: "https://github.com/acme/site/issues/2"},
		{Number: 3, Title: "a PR", State: "open", HTML: "https://github.com/acme/site/pull/3", PullRequest: ghPR()},
	})
	// loom already has a mirror of gh#1
	loomSrv, st := fakeLoom(t, []LoomIssue{
		{Number: 10, Title: "already mirrored", Body: "x\n\n---\nMirrored from ... (mirror:github:acme/site#1)"},
	})

	res, err := MirrorGitHubIssuesToLoom(context.Background(), MirrorIssuesOptions{
		GitHubRepo: "acme/site", GitHubToken: "t", LoomURL: loomSrv.URL, LoomRepo: "loom/site", LoomToken: "lt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 2 { // PR excluded
		t.Fatalf("scanned=%d, want 2 (PR excluded)", res.Scanned)
	}
	if res.Created != 1 {
		t.Fatalf("created=%d, want 1", res.Created)
	}
	byNum := map[int]MirroredIssue{}
	for _, m := range res.Issues {
		byNum[m.GitHubNumber] = m
	}
	if byNum[1].Action != "skipped-existing" {
		t.Fatalf("gh#1 action=%q, want skipped-existing", byNum[1].Action)
	}
	if byNum[2].Action != "created" || byNum[2].LoomNumber == 0 {
		t.Fatalf("gh#2 should be created with a loom number: %+v", byNum[2])
	}
	// the created loom issue carries the marker for gh#2
	found := false
	for _, is := range st.issues {
		if strings.Contains(is.Body, "mirror:github:acme/site#2") {
			found = true
		}
	}
	if !found {
		t.Fatal("created loom issue missing the gh#2 marker")
	}
}

func TestMirrorIdempotent(t *testing.T) {
	fakeGitHub(t, []GitHubIssue{
		{Number: 5, Title: "req", State: "open", HTML: "https://github.com/acme/site/issues/5"},
	})
	loomSrv, _ := fakeLoom(t, nil)
	opt := MirrorIssuesOptions{GitHubRepo: "acme/site", GitHubToken: "t", LoomURL: loomSrv.URL, LoomRepo: "loom/site", LoomToken: "lt"}

	r1, err := MirrorGitHubIssuesToLoom(context.Background(), opt)
	if err != nil || r1.Created != 1 {
		t.Fatalf("first run: created=%d err=%v", r1.Created, err)
	}
	r2, err := MirrorGitHubIssuesToLoom(context.Background(), opt) // re-run: already mirrored
	if err != nil {
		t.Fatal(err)
	}
	if r2.Created != 0 {
		t.Fatalf("second run created=%d, want 0 (idempotent)", r2.Created)
	}
}

func TestMirrorDryRun(t *testing.T) {
	fakeGitHub(t, []GitHubIssue{{Number: 9, Title: "x", State: "open", HTML: "u"}})
	loomSrv, st := fakeLoom(t, nil)
	res, err := MirrorGitHubIssuesToLoom(context.Background(), MirrorIssuesOptions{
		GitHubRepo: "acme/site", GitHubToken: "t", LoomURL: loomSrv.URL, LoomRepo: "loom/site", LoomToken: "lt", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Issues[0].Action != "dry-run" {
		t.Fatalf("dry-run should create nothing: %+v", res)
	}
	if len(st.issues) != 0 {
		t.Fatalf("dry-run created loom issues: %d", len(st.issues))
	}
}

// The owner-driven control labels (sdlc:* / deploy:*) are synced github → loom;
// non-control labels (type:*) are not — so the owner manages the loop from GitHub.
func TestMirrorSyncsControlLabels(t *testing.T) {
	fakeGitHub(t, []GitHubIssue{
		{Number: 1, Title: "req", State: "open", HTML: "u", Labels: lbl("sdlc:go", "type:bug", "deploy:qa")},
	})
	loomSrv, st := fakeLoom(t, nil) // fresh → create + sync control labels
	res, err := MirrorGitHubIssuesToLoom(context.Background(), MirrorIssuesOptions{
		GitHubRepo: "acme/site", GitHubToken: "t", LoomURL: loomSrv.URL, LoomRepo: "loom/site", LoomToken: "lt",
	})
	if err != nil {
		t.Fatal(err)
	}
	added := res.Issues[0].LabelsAdded
	set := map[string]bool{}
	for _, a := range added {
		set[a] = true
	}
	if !set["sdlc:go"] || !set["deploy:qa"] {
		t.Fatalf("control labels not synced: %v", added)
	}
	if set["type:bug"] {
		t.Fatalf("non-control label type:bug must NOT be synced: %v", added)
	}
	if _, ok := st.labels["sdlc:go"]; !ok {
		t.Fatal("sdlc:go label not created in loom repo")
	}
	loomNum := res.Issues[0].LoomNumber
	if len(st.labelAdds[loomNum]) != 2 {
		t.Fatalf("expected 2 control labels added to loom#%d, got %d", loomNum, len(st.labelAdds[loomNum]))
	}
}

func TestMirrorRequiresArgs(t *testing.T) {
	if _, err := MirrorGitHubIssuesToLoom(context.Background(), MirrorIssuesOptions{GitHubRepo: "a/b"}); err == nil {
		t.Fatal("expected error without loom-repo/loom-url")
	}
}
