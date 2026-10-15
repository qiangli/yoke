package quotaobs

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponseSuccessDoesNotReadOrWrapBody(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	body := io.NopCloser(strings.NewReader("private successful body"))
	resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Ratelimit-Reset-Tokens": {"100"}}, Body: body}
	ObserveResponse(context.Background(), resp, Observation{Source: "door"})
	if resp.Body != body {
		t.Fatal("successful body wrapped")
	}
	resp.Header.Set("X-Ratelimit-Reset-Tokens", "200")
	ObserveResponse(context.Background(), resp, Observation{Source: "door"})
	got, err := Read(time.Time{}, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("changed headers lost to dedupe: %+v, %v", got, err)
	}
	for _, o := range got {
		if o.Excerpt != "" {
			t.Fatal("success excerpt")
		}
	}
}

func TestResponseJournalFailurePreservesBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-directory")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_HOME", path)
	resp := &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("quota exceeded"))}
	ObserveResponse(context.Background(), resp, Observation{Source: "door"})
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != "quota exceeded" {
		t.Fatalf("response changed: %q %v", got, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResponseAdmissionNotObserved(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	body := io.NopCloser(strings.NewReader("local admission refusal"))
	resp := &http.Response{StatusCode: 429, Header: http.Header{"X-Bashy-Refusal": {"quota"}, "Retry-After": {"1"}}, Body: body}
	ObserveResponse(context.Background(), resp, Observation{Source: "door"})
	if resp.Body != body {
		t.Fatal("admission body wrapped")
	}
	got, err := Read(time.Time{}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("admission observed: %+v, %v", got, err)
	}
}
