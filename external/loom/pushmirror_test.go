package loom

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeForge is a Gitea source forge that RECORDS every request — the mirror
// tests assert on what was sent (and on what was never sent: the target is
// never provisioned, and the push token must not appear anywhere but the
// create body).
type fakeForge struct {
	mu      sync.Mutex
	repos   map[string]bool
	mirrors map[string][]pushMirror
	syncs   map[string]int
	seen    []string         // "METHOD /path"
	bodies  []string         // raw request bodies, in order
	created []map[string]any // decoded push_mirrors create bodies
	deny401 bool
	n       int
}

func newFakeForge(repos ...string) (*fakeForge, *httptest.Server) {
	f := &fakeForge{repos: map[string]bool{}, mirrors: map[string][]pushMirror{}, syncs: map[string]int{}}
	for _, r := range repos {
		f.repos[r] = true
	}
	return f, httptest.NewServer(f)
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(r.Body)
	f.seen = append(f.seen, r.Method+" "+p+r.URL.RawQuery)
	f.bodies = append(f.bodies, raw.String())
	js := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case strings.HasSuffix(p, "/push_mirrors-sync") && r.Method == "POST":
		f.syncs[strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/push_mirrors-sync")]++
		js(200, map[string]any{})
	case strings.HasSuffix(p, "/push_mirrors") && r.Method == "GET":
		js(200, f.mirrors[strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/push_mirrors")])
	case strings.HasSuffix(p, "/push_mirrors") && r.Method == "POST":
		if f.deny401 {
			js(401, map[string]string{"message": "Permission denied"})
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/push_mirrors")
		var in map[string]any
		_ = json.Unmarshal(raw.Bytes(), &in)
		f.created = append(f.created, in)
		f.n++
		// Gitea answers with the SANITIZED address (no credentials) and names
		// the remote itself.
		f.mirrors[name] = append(f.mirrors[name], pushMirror{
			RemoteName:    "remote_mirror_" + string(rune('a'+f.n-1)),
			RemoteAddress: in["remote_address"].(string),
			SyncOnCommit:  in["sync_on_commit"] == true,
			Interval:      in["interval"].(string),
		})
		js(200, map[string]any{})
	case strings.Contains(p, "/push_mirrors/") && r.Method == "DELETE":
		name := strings.TrimPrefix(strings.SplitN(p, "/push_mirrors/", 2)[0], "/repos/")
		rn := strings.SplitN(p, "/push_mirrors/", 2)[1]
		var keep []pushMirror
		for _, m := range f.mirrors[name] {
			if m.RemoteName != rn {
				keep = append(keep, m)
			}
		}
		f.mirrors[name] = keep
		w.WriteHeader(204)
	case p == "/repos/search":
		var d []repoInfo
		for n := range f.repos {
			d = append(d, repoInfo{FullName: n})
		}
		js(200, map[string]any{"data": d})
	case strings.HasPrefix(p, "/repos/") && r.Method == "GET":
		n := strings.TrimPrefix(p, "/repos/")
		if f.repos[n] {
			js(200, repoInfo{FullName: n})
		} else {
			js(404, "nf")
		}
	default:
		js(404, "nf")
	}
}

func (f *fakeForge) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.seen, "\n") + "\n" + strings.Join(f.bodies, "\n")
}

func tokenFileAt(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

const ghRemote = "https://github.com/acme/app.git"

func mirrorOpts(primary string) MirrorOptions {
	return MirrorOptions{Primary: primary, Token: "src-token", Repo: "acme/app",
		Remote: ghRemote, Username: "acme-bot", Password: "gh-secret", SyncOnCommit: true}
}

func TestMirrorAddDisabled(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	_, err := MirrorAdd(context.Background(), mirrorOpts(s.URL))
	if err == nil || !strings.Contains(err.Error(), "backup add") {
		t.Fatalf("want safe migration error: %v", err)
	}
	if len(f.seen) != 0 {
		t.Fatal("disabled mirror contacted forge")
	}
}

func TestMirrorListAndRm(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	ctx := context.Background()
	f.mirrors["acme/app"] = []pushMirror{{RemoteName: "mirror-1", RemoteAddress: ghRemote}, {RemoteName: "mirror-2", RemoteAddress: "https://gitlab.com/acme/app.git"}}

	lines, err := MirrorList(ctx, MirrorOptions{Primary: s.URL, Repo: "acme/app"})
	if err != nil || len(lines) != 2 {
		t.Fatalf("list = %v %v, want 2", lines, err)
	}
	lines, err = MirrorList(ctx, MirrorOptions{Primary: s.URL, Repo: "acme/app", Remote: ghRemote})
	if err != nil || len(lines) != 1 || !strings.Contains(lines[0], "github.com") {
		t.Fatalf("filtered list = %v %v", lines, err)
	}
	// Repo-less list walks every repo.
	if lines, err = MirrorList(ctx, MirrorOptions{Primary: s.URL}); err != nil || len(lines) != 2 {
		t.Fatalf("whole-forge list = %v %v", lines, err)
	}

	// `rm` matches without the .git suffix, removes only the named target, and
	// is idempotent.
	if lines, err = MirrorRm(ctx, MirrorOptions{Primary: s.URL, Repo: "acme/app", Remote: "https://github.com/acme/app"}); err != nil ||
		len(lines) != 1 || !strings.Contains(lines[0], "removed") {
		t.Fatalf("rm = %v %v", lines, err)
	}
	if got := f.mirrors["acme/app"]; len(got) != 1 || !strings.Contains(got[0].RemoteAddress, "gitlab.com") {
		t.Fatalf("rm removed the wrong mirror: %+v", got)
	}
	if lines, err = MirrorRm(ctx, MirrorOptions{Primary: s.URL, Repo: "acme/app", Remote: ghRemote}); err != nil ||
		len(lines) != 1 || !strings.Contains(lines[0], "no push mirror") {
		t.Fatalf("second rm = %v %v", lines, err)
	}
	for name, o := range map[string]MirrorOptions{
		"no repo":   {Primary: s.URL, Remote: ghRemote},
		"no remote": {Primary: s.URL, Repo: "acme/app"},
	} {
		if _, err := MirrorRm(ctx, o); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

func TestReadMirrorToken(t *testing.T) {
	if got, err := readMirrorToken(tokenFileAt(t, "s3cret\n# a comment\n", 0o600), nil); err != nil || got != "s3cret" {
		t.Fatalf("0600 file -> %q %v", got, err)
	}
	if got, err := readMirrorToken("-", strings.NewReader("piped\n")); err != nil || got != "piped" {
		t.Fatalf("stdin -> %q %v", got, err)
	}
	for name, call := range map[string]func() (string, error){
		"unset":     func() (string, error) { return readMirrorToken("", nil) },
		"missing":   func() (string, error) { return readMirrorToken(filepath.Join(t.TempDir(), "nope"), nil) },
		"directory": func() (string, error) { return readMirrorToken(t.TempDir(), nil) },
		"empty":     func() (string, error) { return readMirrorToken(tokenFileAt(t, "\n", 0o600), nil) },
		"no stdin":  func() (string, error) { return readMirrorToken("-", nil) },
	} {
		if _, err := call(); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

// A token file any account on the host can read defeats the purpose of keeping
// the secret off the command line.
func TestReadMirrorTokenRefusesLooseMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go synthesizes 0666 for every regular file on Windows")
	}
	p := tokenFileAt(t, "s3cret\n", 0o644)
	if _, err := readMirrorToken(p, nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v, want a mode refusal naming the fix", err)
	}
}

func TestMirrorCommandRefusesDestructiveAdd(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	root := NewLoomCmd()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"mirror", "add", "--primary", s.URL, "--repo", "acme/app", "--remote", ghRemote, "--token-file", tokenFileAt(t, "secret", 0600)})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "backup add") {
		t.Fatalf("want migration error, got %v", err)
	}
	if len(f.seen) != 0 {
		t.Fatal("disabled command contacted forge")
	}
}

// The force/prune hazard has to be in the help: an operator who points this at
// a live repo loses the refs the source does not have.
func TestMirrorHelpStatesHazards(t *testing.T) {
	for _, want := range []string{"DELETED", "OVERWRITTEN", "FORCE", "ALREADY EXISTS", "token-file"} {
		if !strings.Contains(mirrorLong, want) {
			t.Fatalf("mirror help does not state %q:\n%s", want, mirrorLong)
		}
	}
	root := NewLoomCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "mirror" {
			found = true
		}
	}
	if !found {
		t.Fatal("mirror is not registered on `loom`")
	}
}
