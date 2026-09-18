package sdlc

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestSyncGitHubResolution_SuccessClosesAndLabels(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	run := RunRecord{IssueID: "acme/app#12", ReferenceID: "ref1"}
	acted, err := SyncGitHubResolution(context.Background(), run, "resolved", true, "done", "tok")
	if err != nil || !acted {
		t.Fatalf("acted=%v err=%v", acted, err)
	}
	// Expect: DELETE in-progress, POST done label, POST comment, PATCH close.
	methods := map[string]int{}
	sawDone := false
	sawClose := false
	for _, r := range *reqs {
		methods[r.method]++
		if r.method == http.MethodPost {
			if labels, ok := r.body["labels"].([]any); ok && len(labels) == 1 && labels[0] == LabelDone {
				sawDone = true
			}
		}
		if r.method == http.MethodPatch && r.body["state"] == "closed" {
			sawClose = true
		}
	}
	if !sawDone {
		t.Fatal("expected sdlc:done label to be applied")
	}
	if !sawClose {
		t.Fatal("expected the issue to be closed")
	}
	if methods[http.MethodDelete] != 1 {
		t.Fatalf("expected the in-progress claim to be removed once, methods=%v", methods)
	}
}

func TestSyncGitHubResolution_RejectedDoesNotClose(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	run := RunRecord{IssueID: "acme/app#5"}
	acted, err := SyncGitHubResolution(context.Background(), run, "rejected", true, "", "tok")
	if err != nil || !acted {
		t.Fatalf("acted=%v err=%v", acted, err)
	}
	for _, r := range *reqs {
		if r.method == http.MethodPatch {
			t.Fatal("rejected resolution must not close the issue")
		}
		if r.method == http.MethodPost {
			if labels, ok := r.body["labels"].([]any); ok && len(labels) == 1 && labels[0] == LabelDone {
				t.Fatal("rejected resolution must not apply sdlc:done")
			}
		}
	}
}

func TestSyncGitHubResolution_NoOpWithoutToken(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	acted, err := SyncGitHubResolution(context.Background(), RunRecord{IssueID: "acme/app#1"}, "resolved", true, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	if acted {
		t.Fatal("should not act without a token")
	}
	if len(*reqs) != 0 {
		t.Fatalf("no HTTP calls expected without token, got %d", len(*reqs))
	}
}

func TestSyncGitHubResolution_NoOpForLocalRun(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	acted, _ := SyncGitHubResolution(context.Background(), RunRecord{IssueID: "local-only"}, "resolved", true, "x", "tok")
	if acted || len(*reqs) != 0 {
		t.Fatalf("non-github run should be a no-op; acted=%v calls=%d", acted, len(*reqs))
	}
}

// The resolve command closes the loop: resolving a github-backed run applies
// sdlc:done and closes the issue.
func TestResolveCommandSyncsGitHub(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	t.Setenv("GITHUB_TOKEN", "tok")
	dir, id := makeApprovedRun(t, "acme/app#12")

	cmd := NewSDLCCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"resolve", id, "--runs-dir", dir, "--status", "resolved"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	sawClose := false
	for _, r := range *reqs {
		if r.method == http.MethodPatch && r.body["state"] == "closed" {
			sawClose = true
		}
	}
	if !sawClose {
		t.Fatalf("resolve should close the github issue; requests=%+v", *reqs)
	}
}
