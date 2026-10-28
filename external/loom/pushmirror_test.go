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

// add is idempotent, carries the credential in the create body, and re-syncs
// an already-mirrored repo (a fresh mirror is EMPTY until something pushes).
func TestMirrorAddIdempotent(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	o := mirrorOpts(s.URL)
	for i := 0; i < 2; i++ {
		lines, err := MirrorAdd(context.Background(), o)
		if err != nil {
			t.Fatalf("add #%d: %v", i, err)
		}
		if len(lines) != 1 || !strings.Contains(lines[0], ghRemote) {
			t.Fatalf("add #%d lines = %v", i, lines)
		}
	}
	if got := f.mirrors["acme/app"]; len(got) != 1 {
		t.Fatalf("mirrors = %+v, want exactly 1 after two adds", got)
	}
	if len(f.created) != 1 {
		t.Fatalf("create bodies = %d, want 1", len(f.created))
	}
	in := f.created[0]
	if in["remote_address"] != ghRemote || in["remote_username"] != "acme-bot" ||
		in["remote_password"] != "gh-secret" || in["sync_on_commit"] != true || in["interval"] != "8h0m0s" {
		t.Fatalf("create body = %+v", in)
	}
	if f.syncs["acme/app"] != 2 {
		t.Fatalf("syncs = %d, want 2 (every add requests one)", f.syncs["acme/app"])
	}
}

// The whole point of this verb: unlike `replica add`, nothing is provisioned on
// the target — no owner, no repo, no admin call anywhere.
func TestMirrorAddNeverProvisions(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	if _, err := MirrorAdd(context.Background(), mirrorOpts(s.URL)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"/admin/users", "/orgs"} {
		if strings.Contains(f.log(), " "+bad) {
			t.Fatalf("provisioning call %q issued:\n%s", bad, f.log())
		}
	}
}

func TestMirrorAddUnknownSourceRepo(t *testing.T) {
	_, s := newFakeForge("acme/other")
	defer s.Close()
	if _, err := MirrorAdd(context.Background(), mirrorOpts(s.URL)); err == nil ||
		!strings.Contains(err.Error(), "source repo") {
		t.Fatalf("err = %v, want a source-repo error", err)
	}
}

func TestMirrorAddValidation(t *testing.T) {
	_, s := newFakeForge("acme/app")
	defer s.Close()
	for name, mut := range map[string]func(*MirrorOptions){
		"no repo":      func(o *MirrorOptions) { o.Repo = "" },
		"no remote":    func(o *MirrorOptions) { o.Remote = "" },
		"no username":  func(o *MirrorOptions) { o.Username = "" },
		"no token":     func(o *MirrorOptions) { o.Password = "" },
		"ssh remote":   func(o *MirrorOptions) { o.Remote = "git@github.com:acme/app.git" },
		"creds in url": func(o *MirrorOptions) { o.Remote = "https://bot:pw@github.com/acme/app.git" },
		"not a repo":   func(o *MirrorOptions) { o.Remote = "https://github.com/acme" },
		"blank repo":   func(o *MirrorOptions) { o.Repo = "   " },
	} {
		o := mirrorOpts(s.URL)
		mut(&o)
		if _, err := MirrorAdd(context.Background(), o); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

// A 401 from push_mirrors creation reads like a credential failure and often is
// not one, so it must name the knob (and the credential) — as replica does.
func TestMirrorAdd401Hint(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	f.deny401 = true
	_, err := MirrorAdd(context.Background(), mirrorOpts(s.URL))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{MirrorAllowedDomainsEnv, "github.com", "--token-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("hint missing %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "gh-secret") {
		t.Fatalf("the push token leaked into the error:\n%s", err)
	}
}

func TestMirrorListAndRm(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	ctx := context.Background()
	o := mirrorOpts(s.URL)
	if _, err := MirrorAdd(ctx, o); err != nil {
		t.Fatal(err)
	}
	other := o
	other.Remote = "https://gitlab.com/acme/app.git"
	if _, err := MirrorAdd(ctx, other); err != nil {
		t.Fatal(err)
	}

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

// End to end through cobra: the token reaches the forge, and appears NOWHERE in
// the argv, the output, or the request log outside the create body.
func TestMirrorCmdKeepsTokenOffArgvAndOutput(t *testing.T) {
	f, s := newFakeForge("acme/app")
	defer s.Close()
	const secret = "ghp_liveSecret123"
	args := []string{"mirror", "add", "--primary", s.URL, "--token", "src-token",
		"--repo", "acme/app", "--remote", ghRemote, "--username", "acme-bot",
		"--token-file", tokenFileAt(t, secret+"\n", 0o600), "--sync-on-commit"}

	root := NewLoomCmd()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if strings.Contains(strings.Join(args, " "), secret) {
		t.Fatal("the secret must never be an argument")
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("the token was echoed:\n%s", out)
	}
	if len(f.created) != 1 || f.created[0]["remote_password"] != secret {
		t.Fatalf("the forge did not receive the token: %+v", f.created)
	}
	// The create body is the only place it may appear.
	f.mu.Lock()
	seen, bodies := strings.Join(f.seen, "\n"), f.bodies
	f.mu.Unlock()
	if strings.Contains(seen, secret) {
		t.Fatalf("the token reached a URL path or query:\n%s", seen)
	}
	hits := 0
	for _, b := range bodies {
		if strings.Contains(b, secret) {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("token appears in %d request bodies, want exactly 1 (the create)", hits)
	}
	if !strings.Contains(out.String(), "mirrored") {
		t.Fatalf("no result line:\n%s", out)
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
