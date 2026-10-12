package feed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestFollowSyncCommands(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		ref        string
		paired     bool
		status     int
		method     string
		body       map[string]string
		wantWarn   string
		wantFollow bool
	}{
		{name: "uid follow", command: "follow", ref: "uid:U", paired: true, method: http.MethodPost, body: map[string]string{"author": "U"}, wantFollow: true},
		{name: "author URL follow", command: "follow", ref: "https://h/u/U/", paired: true, method: http.MethodPost, body: map[string]string{"author": "U"}, wantFollow: true},
		{name: "label follow", command: "follow", ref: "label:x", paired: true, method: http.MethodPost, body: map[string]string{"label": "x"}, wantFollow: true},
		{name: "uid unfollow", command: "unfollow", ref: "uid:U", paired: true, method: http.MethodDelete, body: map[string]string{"author": "U"}},
		{name: "label unfollow", command: "unfollow", ref: "label:x", paired: true, method: http.MethodDelete, body: map[string]string{"label": "x"}},
		{name: "plain sink URL", command: "follow", ref: "https://h/posts", paired: true, wantFollow: true},
		{name: "rclone reference", command: "follow", ref: "remote:posts", paired: true, wantFollow: true},
		{name: "unpaired", command: "follow", ref: "uid:U", wantWarn: "not paired", wantFollow: true},
		{name: "server error", command: "follow", ref: "uid:U", paired: true, status: http.StatusInternalServerError, method: http.MethodPost, body: map[string]string{"author": "U"}, wantWarn: "follow sync failed", wantFollow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type request struct {
				method, path, auth, contentType string
				body                            map[string]string
				decodeErr                       error
			}
			requests := make(chan request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got := request{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type")}
				got.decodeErr = json.NewDecoder(r.Body).Decode(&got.body)
				requests <- got
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(`{"error":"server unavailable"}`))
				} else {
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer server.Close()

			te := newTestEnv(t, nil)
			te.env.HTTP = server.Client()
			te.env.Cloud = func() (string, string) {
				if !tt.paired {
					return server.URL, ""
				}
				return server.URL, "test-token"
			}
			ref, err := NormalizeFollow(tt.ref)
			if err != nil {
				t.Fatal(err)
			}
			if tt.command == "unfollow" {
				if err := SaveFollows(te.env.dir(), []string{ref}); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, err := te.run(tt.command, tt.ref)
			if err != nil {
				t.Fatalf("command failed: %v; stdout=%q stderr=%q", err, stdout, stderr)
			}
			if !strings.Contains(stdout, ref) {
				t.Errorf("stdout %q does not name %q", stdout, ref)
			}
			if tt.wantWarn == "" && stderr != "" || tt.wantWarn != "" && !strings.Contains(stderr, tt.wantWarn) {
				t.Errorf("stderr = %q; want warning %q", stderr, tt.wantWarn)
			}
			follows, err := LoadFollows(te.env.dir())
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantFollow && !reflect.DeepEqual(follows, []string{ref}) || !tt.wantFollow && len(follows) != 0 {
				t.Errorf("follows.json = %q; want followed=%t", follows, tt.wantFollow)
			}
			if tt.method == "" {
				if len(requests) != 0 {
					t.Errorf("unexpected HTTP call: %+v", <-requests)
				}
				return
			}
			if len(requests) != 1 {
				t.Fatalf("HTTP calls = %d, want 1", len(requests))
			}
			got := <-requests
			if got.method != tt.method || got.path != "/api/v1/feed/follows" || got.auth != "Bearer test-token" || got.contentType != "application/json" || got.decodeErr != nil || !reflect.DeepEqual(got.body, tt.body) {
				t.Errorf("request = %+v; want method=%s body=%v", got, tt.method, tt.body)
			}
		})
	}
}

func TestParseFollowSyncTarget(t *testing.T) {
	tests := []struct {
		ref, kind, target string
		ok                bool
	}{
		{ref: "uid:U", kind: "author", target: "U", ok: true},
		{ref: "https://h/u/U/", kind: "author", target: "U", ok: true},
		{ref: "label:x", kind: "label", target: "x", ok: true},
		{ref: "https://h/u/U/?page=2"},
		{ref: "https://h/u/U/posts"},
		{ref: "https://h/u/"},
		{ref: "uid:"},
		{ref: "uid:/"},
		{ref: "uid:U/other"},
		{ref: "https://h/posts"},
		{ref: "remote:posts"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			kind, target, ok := parseFollowSyncTarget(tt.ref)
			if kind != tt.kind || target != tt.target || ok != tt.ok {
				t.Errorf("parseFollowSyncTarget(%q) = %q, %q, %t; want %q, %q, %t", tt.ref, kind, target, ok, tt.kind, tt.target, tt.ok)
			}
		})
	}
}
