package loom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	ygit "github.com/qiangli/yoke/git"
)

type fakeGitea struct {
	mu      sync.Mutex
	repos   map[string]bool // full name -> exists
	orgs    map[string]bool
	mirrors map[string][]pushMirror
	creates int
}

func newFakeGitea(repos ...string) (*fakeGitea, *httptest.Server) {
	f := &fakeGitea{repos: map[string]bool{}, orgs: map[string]bool{}, mirrors: map[string][]pushMirror{}}
	for _, r := range repos {
		f.repos[r] = true
		f.orgs[strings.Split(r, "/")[0]] = true
	}
	return f, httptest.NewServer(f)
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	js := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case p == "/repos/search":
		var d []repoInfo
		for n := range f.repos {
			d = append(d, repoInfo{FullName: n})
		}
		js(200, map[string]any{"data": d})
	case strings.HasSuffix(p, "/push_mirrors") && r.Method == "GET":
		js(200, f.mirrors[strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/push_mirrors")])
	case strings.HasSuffix(p, "/push_mirrors") && r.Method == "POST":
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/push_mirrors")
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["sync_on_commit"] != true {
			js(400, "sync_on_commit required")
			return
		}
		f.mirrors[name] = append(f.mirrors[name], pushMirror{RemoteName: "remote_mirror_x", RemoteAddress: in["remote_address"].(string), SyncOnCommit: true})
		js(200, map[string]any{})
	case strings.Contains(p, "/push_mirrors/") && r.Method == "DELETE":
		name := strings.TrimPrefix(strings.SplitN(p, "/push_mirrors/", 2)[0], "/repos/")
		f.mirrors[name] = nil
		w.WriteHeader(204)
	case strings.HasPrefix(p, "/repos/") && r.Method == "GET":
		n := strings.TrimPrefix(p, "/repos/")
		if f.repos[n] {
			js(200, repoInfo{FullName: n})
		} else {
			js(404, "nf")
		}
	case p == "/orgs" && r.Method == "POST":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.orgs[in["username"]] = true
		js(201, in)
	case strings.HasPrefix(p, "/orgs/") && strings.HasSuffix(p, "/repos") && r.Method == "POST":
		org := strings.Split(p, "/")[2]
		if !f.orgs[org] {
			js(404, "no org")
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.repos[org+"/"+in["name"].(string)] = true
		f.creates++
		js(201, in)
	default:
		js(404, "nf")
	}
}

func TestReplicaAddIdempotent(t *testing.T) {
	prim, ps := newFakeGitea("acme/app", "acme/lib")
	defer ps.Close()
	repl, rs := newFakeGitea()
	defer rs.Close()
	o := ReplicaOptions{Primary: ps.URL, Replica: rs.URL, Token: "t", ReplicaToken: "r"}
	for i := 0; i < 2; i++ {
		if _, err := ReplicaAdd(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if repl.creates != 2 || !repl.repos["acme/app"] || !repl.repos["acme/lib"] || !repl.orgs["acme"] {
		t.Fatalf("replica not ensured: %+v", repl)
	}
	for _, n := range []string{"acme/app", "acme/lib"} {
		ms := prim.mirrors[n]
		if len(ms) != 1 || !ms[0].SyncOnCommit || ms[0].RemoteAddress != rs.URL+"/"+n+".git" {
			t.Fatalf("%s mirrors = %+v", n, ms)
		}
	}
	// --repo scoping
	o.Repo = "acme/app"
	lines, err := ReplicaList(context.Background(), o)
	if err != nil || len(lines) != 1 {
		t.Fatalf("list: %v %v", lines, err)
	}
	if _, err := ReplicaRm(context.Background(), ReplicaOptions{Primary: ps.URL, Replica: rs.URL}); err != nil {
		t.Fatal(err)
	}
	if len(prim.mirrors["acme/app"]) != 0 || len(prim.mirrors["acme/lib"]) != 0 {
		t.Fatal("mirrors not removed")
	}
}

func seedRepo2(t *testing.T, dir string) {
	t.Helper()
	w, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644)
	wt, _ := w.Worktree()
	_, _ = wt.Add("a")
	if _, err := wt.Commit("c", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
}

func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func TestCloneAndRefreshFailover(t *testing.T) {
	root := t.TempDir()
	primRoot := filepath.Join(root, "primary")
	r1 := filepath.Join(root, "r1")
	r2 := filepath.Join(root, "r2")
	for _, base := range []string{primRoot, r1, r2} {
		seedRepo2(t, filepath.Join(base, "acme", "app.git"))
	}
	primary := fileURL(filepath.Join(primRoot, "acme", "app.git"))
	replicas := []string{fileURL(filepath.Join(root, "down")), fileURL(r1), fileURL(r2)}
	ctx := context.Background()
	dir := filepath.Join(root, "work")
	from, err := Clone(ctx, primary, replicas, dir, ygit.AuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if want := fileURL(r1) + "/acme/app.git"; from != want {
		t.Fatalf("cloned from %s want %s", from, want)
	}
	assertRemote(t, dir, fileURL(r1)+"/acme/app.git", primary)

	if err := os.RemoveAll(r1); err != nil {
		t.Fatal(err)
	}
	if got, err := RemoteRefresh(ctx, dir); err != nil || got != fileURL(r2)+"/acme/app.git" {
		t.Fatalf("refresh -> %q %v", got, err)
	}
	assertRemote(t, dir, fileURL(r2)+"/acme/app.git", primary)

	if err := os.RemoveAll(r2); err != nil {
		t.Fatal(err)
	}
	if got, err := RemoteRefresh(ctx, dir); err != nil || got != primary {
		t.Fatalf("refresh -> %q %v", got, err)
	}
	assertRemote(t, dir, primary, primary)
}

func assertRemote(t *testing.T, dir, wantURL, wantPush string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "url = "+wantURL+"\n") || !strings.Contains(s, "pushurl = "+wantPush+"\n") {
		t.Fatalf("config want url=%s pushurl=%s:\n%s", wantURL, wantPush, s)
	}
	if strings.Count(s, "replica = ") != 3 {
		t.Fatalf("replicas not persisted:\n%s", s)
	}
}
