package feed

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type publicDirSink struct{ dirSink }

func (d *publicDirSink) Put(ctx context.Context, rel string, data []byte) (string, error) {
	_, err := d.dirSink.Put(ctx, rel, data)
	return "https://feed.example.test/" + rel, err
}

func TestGroupCLIAndEncryptedFetch(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te.env.OpenSink = func(c SinkConfig) (Sink, error) { return &publicDirSink{dirSink{name: c.Name, root: c.Target}}, nil }
	other, _ := ecdh.X25519().NewPrivateKey(fixedBytes(33, 64))
	removed, _ := ecdh.X25519().NewPrivateKey(fixedBytes(65, 96))
	var group FeedGroup
	var ownerWrapped string
	var addWrapped string
	var rotated map[string]string
	var indexed map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("auth: %q", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == "PUT" && path == "/api/v1/feed/enckey":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["pubkey"] == "" {
				t.Error("missing registered pubkey")
			}
			w.WriteHeader(200)
		case r.Method == "POST" && path == "/api/v1/feed/groups":
			var body struct{ ID, Name, Title, Preset, Wrapped string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Preset != "private" || body.ID == "" || body.Wrapped == "" {
				t.Errorf("create body: %+v", body)
			}
			group = FeedGroup{ID: body.ID, Name: body.Name, Title: body.Title, Read: "members", Epoch: 1, Members: []FeedGroupMember{{UserID: "owner", Role: "owner"}, {UserID: "removed", Role: "member"}}}
			ownerWrapped = body.Wrapped
			_ = json.NewEncoder(w).Encode(group)
		case r.Method == "GET" && path == "/api/v1/feed/groups":
			_ = json.NewEncoder(w).Encode([]FeedGroup{group})
		case r.Method == "GET" && path == "/api/v1/feed/groups/"+group.ID:
			_ = json.NewEncoder(w).Encode(group)
		case r.Method == "GET" && path == "/api/v1/feed/enckey/other":
			_ = json.NewEncoder(w).Encode(map[string]string{"uid": "other", "pubkey": EncPubkeyB64(other)})
		case r.Method == "GET" && path == "/api/v1/feed/enckey/owner":
			owner, _ := LoadOrCreateEncKey(te.env.dir())
			_ = json.NewEncoder(w).Encode(map[string]string{"uid": "owner", "pubkey": EncPubkeyB64(owner)})
		case r.Method == "POST" && path == "/api/v1/feed/groups/"+group.ID+"/members":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			addWrapped = body["wrapped"]
			if body["uid"] != "other" || addWrapped == "" {
				t.Errorf("add body: %+v", body)
			}
			group.Members = append(group.Members, FeedGroupMember{UserID: "other", Role: "member"})
			w.WriteHeader(200)
		case r.Method == "DELETE" && path == "/api/v1/feed/groups/"+group.ID+"/members/removed":
			var body struct {
				Epoch int               `json:"epoch"`
				Keys  map[string]string `json:"keys"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Epoch != 2 || len(body.Keys) != 2 || body.Keys["removed"] != "" {
				t.Errorf("remove body: %+v", body)
			}
			rotated = body.Keys
			group.Epoch = 2
			group.Members = []FeedGroupMember{{UserID: "owner", Role: "owner"}, {UserID: "other", Role: "member"}}
			w.WriteHeader(200)
		case r.Method == "GET" && path == "/api/v1/feed/groups/"+group.ID+"/keys":
			_ = json.NewEncoder(w).Encode([]FeedGroupKey{{Epoch: 1, Wrapped: ownerWrapped}, {Epoch: 2, Wrapped: rotated["owner"]}})
		case r.Method == "POST" && path == "/api/v1/feed/registry":
			_ = json.NewDecoder(r.Body).Decode(&indexed)
			_ = json.NewEncoder(w).Encode(IndexResponse{Indexed: "ok"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	te.env.Cloud = func() (string, string) { return server.URL, "test-token" }
	out := te.mustRun("group", "create", "test-group", "--private", "--title", "Test Group")
	id := strings.Split(out, "\t")[0]
	if id == "" || id != group.ID {
		t.Fatalf("create output %q", out)
	}
	if got := te.mustRun("group", "ls"); !strings.Contains(got, id) {
		t.Fatalf("ls %q", got)
	}
	key1, err := LoadGroupKey(te.env.dir(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unwrap(ownerWrapped, removed, id, 1); err == nil {
		t.Fatal("other member opened owner's wrap")
	}
	te.mustRun("group", "add", id, "other")
	if opened, err := Unwrap(addWrapped, other, id, 1); err != nil || string(opened) != string(key1) {
		t.Fatalf("add wrap: %v", err)
	}
	te.mustRun("group", "rm", id, "removed")
	if _, err := Unwrap(rotated["owner"], removed, id, 2); err == nil {
		t.Fatal("removed member opened new epoch")
	}
	sinkDir := t.TempDir()
	te.mustRun("sink", "add", "rclone", "local", sinkDir)
	te.mustRun("post", writeBody(t, "hello private post"), "--headline", "secret headline", "--dek", "secret dek", "--group", id)
	raw, err := os.ReadFile(firstPostFile(t, sinkDir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret headline") || strings.Contains(string(raw), "hello private post") {
		t.Fatal("plaintext leaked into sink")
	}
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatal(err)
	}
	if post.Group != id || post.Epoch != 2 || post.Enc != "v1" {
		t.Fatalf("post metadata: %+v", post)
	}
	if err := VerifyPost(post); err != nil {
		t.Fatal(err)
	}
	if indexed == nil {
		t.Fatal("group post was not indexed")
	}
	row := indexed["post"].(map[string]any)
	if row["group"] != id || row["epoch"] != float64(2) {
		t.Fatalf("indexed group fields: %+v", row)
	}
	te.mustRun("follow", sinkDir)
	got := te.mustRun("fetch", "--body")
	if !strings.Contains(got, "secret headline") || !strings.Contains(got, "hello private post") {
		t.Fatalf("decrypted fetch %q", got)
	}
	if err := os.Remove(filepath.Join(te.env.dir(), "groups", id+".json")); err != nil {
		t.Fatal(err)
	}
	te.env.Cloud = func() (string, string) { return "", "" }
	got = te.mustRun("fetch", "--unread", "--body")
	if !strings.Contains(got, "[encrypted: group "+id+" epoch 2]") {
		t.Fatalf("missing-key fetch %q", got)
	}
}

func TestPublicGroupPostStaysPlaintext(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/feed/groups":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["preset"] != "public" || body["wrapped"] != nil {
				t.Errorf("public create: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(FeedGroup{ID: "g-public", Name: "public", Read: "anyone", Epoch: 0})
		case r.Method == "GET" && r.URL.Path == "/api/v1/feed/groups/g-public":
			_ = json.NewEncoder(w).Encode(FeedGroup{ID: "g-public", Name: "public", Read: "anyone", Epoch: 0})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	te.env.Cloud = func() (string, string) { return server.URL, "test-token" }
	if got := te.mustRun("group", "create", "public", "--public"); !strings.Contains(got, "g-public") {
		t.Fatalf("create %q", got)
	}
	sinkDir := t.TempDir()
	te.mustRun("sink", "add", "rclone", "local", sinkDir)
	te.mustRun("post", writeBody(t, "hello public"), "--headline", "public headline", "--dek", "public dek", "--group", "g-public", "--no-index")
	raw, err := os.ReadFile(firstPostFile(t, sinkDir))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePost(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Group != "g-public" || p.Epoch != 0 || p.Enc != "" || p.Body != "hello public" || p.Headline != "public headline" {
		t.Fatalf("public post: %+v", p)
	}
	if err := VerifyPost(p); err != nil {
		t.Fatal(err)
	}
}
