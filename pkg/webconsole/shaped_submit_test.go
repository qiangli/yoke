package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/issue"
	core "github.com/qiangli/yoke/pkg/shape"
	state "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/websession"
)

type submitWebEnv struct {
	home       string
	source     string
	options    Options
	handler    http.Handler
	caller     core.Caller
	service    core.Service
	candidate  core.Candidate
	alice, bob http.Header
}

func newSubmitWebEnv(t *testing.T) *submitWebEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("production fixture checks require /bin/sh")
	}
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", home)
	source, _ := productionNotesSource(t, home)
	shapeOpts := productionShapeOptions()
	shapeOpts.Home = home
	shapeOpts.WorkRoot = filepath.Join(home, "shape-work")
	shapeOpts.Runner = []string{"/bin/sh"}
	shapeOpts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	shapeOpts.Activator = &productionActivator{home: home}
	opts := Options{RequireLogin: true, Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")), Pairing: true, PairStorePath: filepath.Join(home, "pairing.json"), Panels: []Panel{{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source, Auth: AuthSystem, Source: "registered", Available: true}}, Shape: shapeOpts}
	h := newTestHandler(t, opts)
	pairs := newPairStore(opts.PairStorePath)
	caller := core.Caller{Principal: "device:alice", Actor: "device:alice", RepresentedUser: "device:alice"}
	svc := (&shapedApp{name: "notes", repo: source, opts: shapeOpts}).candidateService()
	from := t.TempDir()
	if e := copyTree(source, from); e != nil {
		t.Fatal(e)
	}
	index := filepath.Join(from, "public", "index.html")
	b, e := os.ReadFile(index)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(index, append(b, []byte("\n<!-- submit fixture -->\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := svc.Import(context.Background(), caller, core.Wish{Request: "Add a comment to the page"}, from)
	if e != nil {
		t.Fatal(e)
	}
	c, e = svc.Approve(context.Background(), caller, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	return &submitWebEnv{home: home, source: source, options: opts, handler: h, caller: caller, service: svc, candidate: c, alice: pairedPrincipal(t, h, pairs, "alice"), bob: pairedPrincipal(t, h, pairs, "bob")}
}
func (e *submitWebEnv) post(t *testing.T, who http.Header, body string, want int) string {
	t.Helper()
	headers := who.Clone()
	headers.Set("Content-Type", "application/json")
	w := send(e.handler, http.MethodPost, "/api/shape/notes/submit", "192.0.2.20:5555", headers, body)
	if w.Code != want {
		t.Fatalf("submit HTTP %d want %d: %s", w.Code, want, w.Body.String())
	}
	return w.Body.String()
}
func TestShapeSubmitHTTPMatchesLocalAndDoesNotActivate(t *testing.T) {
	e := newSubmitWebEnv(t)
	store, err := state.OpenAt(e.home, "notes", e.caller.Principal)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainOpen(e.source)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Head()
	var got core.Submission
	if err = json.Unmarshal([]byte(e.post(t, e.alice, `{"rev":`+jsonQuote(e.candidate.Rev)+`}`, http.StatusOK)), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.PR == "" || got.Run.ID == 0 || got.SourceRevision != e.candidate.Rev || got.Principal != e.caller.Principal {
		t.Fatalf("bad submission %+v", got)
	}
	local, err := e.service.Submit(context.Background(), e.caller, e.candidate.ID, core.SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if local.ID != got.ID || local.PR != got.PR || local.Run.ID != got.Run.ID || local.ProposalRevision != got.ProposalRevision {
		t.Fatalf("local/web mismatch %+v %+v", local, got)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	afterHead, _ := repo.Head()
	if before.ActiveRevision != after.ActiveRevision || head.Hash() != afterHead.Hash() {
		t.Fatal("submission changed deployment")
	}
	commit, err := repo.CommitObject(plumbing.NewHash(got.ProposalRevision))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.Message, "Shape: notes@"+e.candidate.Rev) || !strings.Contains(commit.Message, "Shaped-By: device:alice") || !strings.Contains(commit.Message, "Shape-Item: "+got.Item) {
		t.Fatal("missing provenance", commit.Message)
	}
	pr, err := src.GetPR(e.home, got.PR)
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadSHA != got.ProposalRevision || pr.Shape == nil || pr.Shape.Run != got.Run.ID {
		t.Fatalf("proposal linkage %+v", pr)
	}
	history := do(e.handler, http.MethodGet, "/api/shape/notes/history", "192.0.2.20:5555", e.alice)
	var view historyView
	if err = json.Unmarshal(history.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range view.Versions {
		if v.Revision == e.candidate.Rev {
			found = v.Submittable
		}
	}
	if !found {
		t.Fatal("approved version has no submit eligibility")
	}
	if view.BaseVersion != nil && view.BaseVersion.Submittable {
		t.Fatal("base is submittable")
	}
}
func TestShapeSubmitHTTPRejectsScopeAndBodyOverrides(t *testing.T) {
	e := newSubmitWebEnv(t)
	e.post(t, e.bob, `{"rev":`+jsonQuote(e.candidate.Rev)+`}`, http.StatusConflict)
	e.post(t, e.alice, `{"rev":`+jsonQuote(e.candidate.Rev)+`,"story":"other-story"}`, http.StatusForbidden)
	for _, extra := range []string{`"tracking_root":"outside"`, `"principal":"device:bob"`, `"actor":"someone"`} {
		e.post(t, e.alice, `{"rev":`+jsonQuote(e.candidate.Rev)+`,`+extra+`}`, http.StatusBadRequest)
	}
	e.post(t, e.alice, `{"rev":""}`, http.StatusBadRequest)
	e.post(t, e.alice, `{"rev":`+jsonQuote(e.candidate.Rev)+`} {}`, http.StatusBadRequest)
	files, err := filepath.Glob(filepath.Join(e.home, "src", ".shape-submissions", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatal("denied request created a submission", files)
	}
	if w := send(e.handler, http.MethodPost, "/api/shape/notes/submit", "192.0.2.20:5555", http.Header{"Content-Type": []string{"application/json"}}, `{"rev":`+jsonQuote(e.candidate.Rev)+`}`); w.Code == http.StatusOK {
		t.Fatal("anonymous submission accepted")
	}
}

func TestShapeSubmitOperatorCanSelectStory(t *testing.T) {
	e := newSubmitWebEnv(t)
	oldLookup := osUserLookup
	osUserLookup = func() string { return "test-operator" }
	defer func() { osUserLookup = oldLookup }()
	caller := core.Caller{Principal: "os:test-operator", Actor: "os:test-operator", RepresentedUser: "os:test-operator"}
	from := t.TempDir()
	if err := copyTree(e.source, from); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(from, "public", "index.html")
	b, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(index, append(b, []byte("\n<!-- operator change -->\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := e.service.Import(context.Background(), caller, core.Wish{Request: "Operator page comment"}, from)
	if err != nil {
		t.Fatal(err)
	}
	c, err = e.service.Approve(context.Background(), caller, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(e.source)
	store := &issue.Store{Root: e.source, Sub: "docs/todo"}
	story := &issue.Issue{ID: "abc123def456", Title: "Review a shaped change", Kind: issue.KindTask, Status: "todo", Seq: 8, Sprint: 564, SprintID: "0f09b646-9acd-5bec-849d-2cf3b0e4edef"}
	if _, err = store.Save(story); err != nil {
		t.Fatal(err)
	}
	token, err := e.options.Sessions.Mint("test-operator")
	if err != nil {
		t.Fatal(err)
	}
	operator := withCookie(&http.Cookie{Name: sessionCookie, Value: token})
	var got core.Submission
	if err = json.Unmarshal([]byte(e.post(t, operator, `{"rev":`+jsonQuote(c.Rev)+`,"story":"abc123def456"}`, http.StatusOK)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Item != "todo:"+story.ID || got.TrackingRoot != store.Dir() {
		t.Fatalf("wrong story %+v", got)
	}
	repo, err := gogit.PlainOpen(e.source)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(got.ProposalRevision))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.Message, "Story-ID: "+story.ID) || !strings.Contains(commit.Message, "Sprint: #564") {
		t.Fatal("missing story provenance", commit.Message)
	}
}
